package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var preparePath string
	var cookPath string
	var tags string
	flag.StringVar(&preparePath, "prepare", "", "Prepares a recipe with information on dependencies and writes it to the file")
	flag.StringVar(&cookPath, "cook", "", "Builds all the dependencies specified by the recipe file")
	flag.StringVar(&tags, "tags", "", "Sets the -tags flag to use with 'go build'. Only affects -cook")

	flag.Parse()

	if (preparePath == "") == (cookPath == "") {
		return errors.New("error: Must provide exactly one of -prepare or -cook")
	}
	if preparePath != "" && tags != "" {
		return errors.New("error: Cannot specify -tags with -prepare")
	}

	if preparePath != "" {
		return runPrepare(preparePath)
	} else {
		return runCook(cookPath, tags)
	}
}

type recipe struct {
	ImportGroups []importGroup `json:"importGroups"`
	GoMod        string        `json:"go.mod"`
	GoSum        string        `json:"go.sum"`
}

type importGroup struct {
	BuildConstraints string   `json:"buildConstraints,omitempty"`
	Packages         []string `json:"packages"`
}

func runCook(recipePath string, tags string) error {
	recipeJSON, err := os.ReadFile(recipePath)
	if err != nil {
		return fmt.Errorf("could not read recipe at %s: %w", recipePath, err)
	}
	var r recipe
	if err := json.Unmarshal(recipeJSON, &r); err != nil {
		return fmt.Errorf("could not unmarshal recipe JSON at %s: %w", recipePath, err)
	}

	// Write go.mod, go.sum, generate main.go file(s), and then run 'go build -o /dev/null .'
	if err := os.WriteFile("go.mod", []byte(r.GoMod), 0o666); err != nil {
		return fmt.Errorf("could not write go.mod: %w", err)
	}
	if err := os.WriteFile("go.sum", []byte(r.GoSum), 0o666); err != nil {
		return fmt.Errorf("could not write go.sum: %w", err)
	}
	var goFiles []string
	for i, g := range r.ImportGroups {
		var filename string
		if i == 0 {
			filename = "main.go"
		} else {
			filename = fmt.Sprintf("main%d.go", i)
		}
		goFiles = append(goFiles, filename)

		var mainContent []byte
		if g.BuildConstraints != "" {
			mainContent = append(mainContent, []byte(fmt.Sprintf("//go:build %s\n\n", g.BuildConstraints))...)
		}

		mainContent = append(mainContent, []byte("package main\n\nimport (\n")...)
		for _, imp := range g.Packages {
			mainContent = append(mainContent, []byte(fmt.Sprintf("\t_ %q\n", imp))...)
		}
		mainContent = append(mainContent, []byte(")\n")...)
		if i == 0 {
			mainContent = append(mainContent, []byte("\nfunc main() {}\n")...)
		}
		if err := os.WriteFile(filename, mainContent, 0o666); err != nil {
			return fmt.Errorf("could not write %s: %w", filename, err)
		}
	}

	args := []string{"build", "-o", "/dev/null"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".") // build the current directory
	goBuild := exec.Command("go", args...)
	goBuild.Stdout = os.Stdout
	goBuild.Stderr = os.Stderr

	if err := goBuild.Run(); err != nil {
		return fmt.Errorf("could not run 'go build' command: %w", err)
	}

	var cleanupErrs []error
	for _, filename := range goFiles {
		cleanupErrs = append(cleanupErrs, os.Remove(filename))
	}
	return errors.Join(cleanupErrs...)
}

func runPrepare(recipePath string) error {
	// Parse the go.mod file to get the name of the module -- that way, we can filter out packages
	// that are *not* part of this one.
	modContents, err := os.ReadFile("go.mod")
	if err != nil {
		return fmt.Errorf("could not read go.mod: %w", err)
	}
	mf, err := modfile.Parse("go.mod", modContents, nil)
	if err != nil {
		return fmt.Errorf("could not parse go.mod: %w", err)
	}
	// name of the module, like 'github.com/foo/bar' or 'example.com/baz'
	moduleName := mf.Module.Mod.Path

	// Read the contents of go.sum, just to store it for later.
	sumContents, err := os.ReadFile("go.sum")
	if err != nil {
		return fmt.Errorf("could not read go.sum: %w", err)
	}

	builder := newImportsBuilder(moduleName)

	err = fs.WalkDir(os.DirFS("."), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		filename := d.Name()
		// Skip hidden files/directories
		if strings.HasPrefix(filename, ".") && filename != "." {
			if d.IsDir() {
				return fs.SkipDir
			} else {
				return nil
			}
		}
		// Parse all files ending in ".go":
		if !d.IsDir() && strings.HasSuffix(filename, ".go") {
			if err := builder.addFile(path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("could not walk dir: %w", err)
	}

	r := recipe{
		ImportGroups: builder.importGroups(),
		GoMod:        string(modContents),
		GoSum:        string(sumContents),
	}
	recipeJSON, err := json.Marshal(&r)
	if err != nil {
		panic(fmt.Errorf("failed to marshal recipe JSON: %w", err))
	}

	if err := os.WriteFile(recipePath, recipeJSON, 0o777); err != nil {
		return fmt.Errorf("could not write recipe to file %s: %w", recipePath, err)
	}

	return nil
}

type importsBuilder struct {
	modPrefix string
	imports   map[string]map[string]struct{}
}

func newImportsBuilder(modName string) *importsBuilder {
	return &importsBuilder{
		modPrefix: fmt.Sprintf("%s/", modName),
		imports:   make(map[string]map[string]struct{}),
	}
}

func (b *importsBuilder) addFile(filepath string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath, nil, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return fmt.Errorf("failed to parse file at %q: %w", filepath, err)
	}

	// Fast path: don't do anything if the file doesn't import anything
	if len(file.Imports) == 0 {
		return nil
	}

	// figure out which import group is accurate for this file based on whether it has a //go:build comment
	buildConstraints := extractBuildConstraints(file)

	ig := b.imports[buildConstraints]
	if ig == nil {
		ig = make(map[string]struct{})
	}

	for _, spec := range file.Imports {
		pkg, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("failed to unquote %s : %w", spec.Path.Value, err)
		}
		if !strings.HasPrefix(pkg, b.modPrefix) {
			ig[pkg] = struct{}{}
		}
	}

	b.imports[buildConstraints] = ig

	return nil
}

// https://pkg.go.dev/cmd/go#hdr-Build_constraints
func extractBuildConstraints(file *ast.File) string {
	buildPrefix := "//go:build "
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, buildPrefix) {
				return strings.TrimPrefix(c.Text, buildPrefix)
			}
		}
	}
	return "" // no build constraints
}

func (b *importsBuilder) importGroups() []importGroup {
	// we're sorting the lists before returning so that this method is deterministic

	var groups []importGroup
	for buildConstraints, group := range b.imports {
		var pkgs []string
		for pkgName := range group {
			pkgs = append(pkgs, pkgName)
		}
		slices.Sort(pkgs)
		groups = append(groups, importGroup{
			BuildConstraints: buildConstraints,
			Packages:         pkgs,
		})
	}

	slices.SortFunc(groups, func(gx, gy importGroup) int {
		if gx.BuildConstraints < gy.BuildConstraints {
			return -1
		} else {
			return 1
		}
	})

	return groups
}
