package main

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/template"

	"github.com/docker/buildx/builder"
	cbuild "github.com/docker/buildx/controller/build"
	buildxpb "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/util/progress"
	dockercli "github.com/docker/cli/cli/command"
	dockerflags "github.com/docker/cli/cli/flags"
	"github.com/moby/buildkit/util/progress/progressui"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
	// note: have import these so that the drivers are available to use.
	_ "github.com/docker/buildx/driver/docker"
	_ "github.com/docker/buildx/driver/docker-container"
)

// TestCase defines a single test case
type TestCase struct {
	Name    string   `yaml:"name"`
	Git     GitInfo  `yaml:"git"`
	Targets []string `yaml:"targets"`
}

type GitInfo struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
}

func main() {
	if err := doMain(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err.Error())
		os.Exit(1)
	}
}

func doMain() error {
	testsFile := flag.String("test-file", "", "Set the file to read test cases from")
	run := flag.String("run", "", "Set specific test case to run")
	flag.Parse()

	if *testsFile == "" {
		return fmt.Errorf("'-test-file' must be provided")
	}

	// Read test cases
	testsFileContents, err := os.ReadFile(*testsFile)
	if err != nil {
		return fmt.Errorf("failed to read tests file %q: %w", *testsFile, err)
	}
	var cases []TestCase
	err = yaml.Unmarshal(testsFileContents, &cases)
	if err != nil {
		return fmt.Errorf("failed to parse tests file %q: %w", *testsFile, err)
	}
	// Check no duplicate names, and build lookup map for later
	testCasesByName := make(map[string]*TestCase)
	for i := range cases {
		if _, ok := testCasesByName[cases[i].Name]; ok {
			return fmt.Errorf("more than one test case named named %q", cases[i].Name)
		}
		testCasesByName[cases[i].Name] = &cases[i]
	}

	dockerCli, err := dockercli.NewDockerCli()
	if err != nil {
		return fmt.Errorf("failed to create docker CLI: %w", err)
	}
	if err := dockerCli.Initialize(&dockerflags.ClientOptions{}); err != nil {
		return fmt.Errorf("failed to initialize docker CLI: %w", err)
	}

	isatty := term.IsTerminal(int(os.Stdout.Fd()))
	ctx := context.TODO()

	if *run != "" /* find the test case we're supposed to run */ {
		testCase, ok := testCasesByName[*run]
		if !ok {
			return fmt.Errorf("could not find test case named %q in tests file %q", testCase.Name, *testsFile)
		}

		succeeded, err := testCase.run(ctx, dockerCli, isatty)
		if err != nil {
			return fmt.Errorf("unexpected error running %q: %w", testCase.Name, err)
		}
		if !succeeded {
			os.Exit(1)
		}
	} else /* run all test cases */ {
		hadFailure := false
		for i := range cases {
			succeeded, err := cases[i].run(ctx, dockerCli, isatty)
			if err != nil {
				return fmt.Errorf("unexpected error running %q: %w", cases[i].Name, err)
			}
			hadFailure = hadFailure || !succeeded
		}
		if hadFailure {
			os.Exit(1)
		}
	}

	return nil
}

func (c *TestCase) run(
	ctx context.Context,
	dockerCli *dockercli.DockerCli,
	isatty bool,
) (ok bool, _ error) {
	var bold, green, red, reset string
	if isatty {
		bold = "\x1b[1m"
		green = "\x1b[32m"
		red = "\x1b[31m"
		reset = "\x1b[0m"
	}

	fmt.Printf("%s%s ...%s\n", bold, c.Name, reset)

	dockerfile, err := c.generateDockerfile()
	if err != nil {
		return false, fmt.Errorf("failed to generate Dockerfile: %w", err)
	}

	mainDotGo, err := os.ReadFile("main.go")
	if err != nil {
		return false, fmt.Errorf("failed to read main.go: %w", err)
	}
	goDotMod, err := os.ReadFile("go.mod")
	if err != nil {
		return false, fmt.Errorf("failed to read go.mod: %w", err)
	}
	goDotSum, err := os.ReadFile("go.sum")
	if err != nil {
		return false, fmt.Errorf("failed to read go.sum: %w", err)
	}

	files := []fileInfo{
		{
			path:     "Dockerfile",
			mode:     0o644,
			contents: dockerfile,
		},
		{
			path:     "main.go",
			mode:     0o644,
			contents: mainDotGo,
		},
		{
			path:     "go.mod",
			mode:     0o644,
			contents: goDotMod,
		},
		{
			path:     "go.sum",
			mode:     0o644,
			contents: goDotSum,
		},
	}

	res, err := doBuild(ctx, dockerCli, files)
	if err != nil {
		return false, fmt.Errorf("failed to run the build: %w", err)
	}

	if res.err == nil /* build was successful */ {
		fmt.Printf("%s%s %s√%s\n", bold, c.Name, green, reset)
		return true, nil
	} else /* we were able to set up the build, and it failed */ {
		fmt.Printf("%s%s %sX%s\n", bold, c.Name, red, reset)
		fmt.Printf(" -> %s\n", res.err.Error())
		return false, nil
	}
}

const templateDockerfilePath = "testrunner/template.Dockerfile"

func (c *TestCase) generateDockerfile() ([]byte, error) {
	dockerfileTemplate, err := os.ReadFile(templateDockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template dockerfile at %q: %w", templateDockerfilePath, err)
	}
	tmpl, err := template.New("Dockerfile").Parse(string(dockerfileTemplate))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template dockerfile: %w", err)
	}

	type targetSpec struct {
		StageSuffix string
		MainPath    string
	}

	type tmplContext struct {
		GoBaseImage  string
		GitBaseImage string
		InstallGit   string
		HostDir      string
		GitURL       string
		GitBranch    string
		Targets      []targetSpec
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	var targets []targetSpec
	for i, path := range c.Targets {
		targets = append(targets, targetSpec{
			StageSuffix: strconv.Itoa(i),
			MainPath:    path,
		})
	}

	tmplArgs := tmplContext{
		GoBaseImage:  "golang:1.23-alpine",
		GitBaseImage: "alpine:3.20",
		InstallGit:   "apk add git",
		HostDir:      cwd,
		GitURL:       c.Git.URL,
		GitBranch:    c.Git.Branch,
		Targets:      targets,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, tmplArgs); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.Bytes(), nil
}

type buildResult struct {
	err error
}

func doBuild(ctx context.Context, dockerCli *dockercli.DockerCli, files []fileInfo) (*buildResult, error) {
	progressMode := progressui.AutoMode

	tarContents, err := filesToTar(files)
	if err != nil {
		return nil, fmt.Errorf("failed to create tar for files: %w", err)
	}

	b, err := builder.New(dockerCli, builder.WithContextPathHash(dockerCli.CurrentContext()))
	if err != nil {
		return nil, fmt.Errorf("failed to create builder client: %w", err)
	}
	_, err = b.LoadNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load builder nodes: %w", err)
	}

	var printer *progress.Printer
	printer, err = progress.NewPrinter(
		ctx,
		os.Stderr,
		progressMode,
		progress.WithDesc(
			fmt.Sprintf("building with %q instance using %s driver", b.Name, b.Driver),
			fmt.Sprintf("%s:%s", b.Driver, b.Name),
		),
		progress.WithOnClose(func() {
			printWarnings(os.Stderr, printer.Warnings(), progressMode)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create printer: %w", err)
	}

	input := tarContents
	buildOpts := buildxpb.BuildOptions{
		ContextPath: "-", // signal that we should read from stdin
	}
	_, _, buildErr := cbuild.RunBuild(ctx, dockerCli, buildOpts, input, printer, false)

	if err := printer.Wait(); err != nil {
		return nil, fmt.Errorf("failed to printer.Wait(): %w", err)
	}

	return &buildResult{
		err: buildErr,
	}, nil
}

type fileInfo struct {
	path     string
	mode     int64
	contents []byte
}

func filesToTar(files []fileInfo) (io.Reader, error) {
	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	defer tw.Close()

	for _, f := range files {
		tarHeader := &tar.Header{
			Name: f.path,
			Size: int64(len(f.contents)),
			Mode: f.mode,
		}

		if err := tw.WriteHeader(tarHeader); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %q: %w", f.path, err)
		}
		if _, err := tw.Write(f.contents); err != nil {
			return nil, fmt.Errorf("failed to write file content for %q: %w", f.path, err)
		}
	}

	return tarBuffer, nil
}
