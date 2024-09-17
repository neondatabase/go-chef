package main

import (
	"bytes"
	"fmt"
	"io"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/morikuni/aec"
	"github.com/sirupsen/logrus"
)

// copied from github.com/docker/buildx commands/build.go
func printWarnings(w io.Writer, warnings []client.VertexWarning, mode progressui.DisplayMode) {
	if len(warnings) == 0 || mode == progressui.QuietMode || mode == progressui.RawJSONMode {
		return
	}
	fmt.Fprintf(w, "\n ")
	sb := &bytes.Buffer{}
	if len(warnings) == 1 {
		fmt.Fprintf(sb, "1 warning found")
	} else {
		fmt.Fprintf(sb, "%d warnings found", len(warnings))
	}
	if logrus.GetLevel() < logrus.DebugLevel {
		fmt.Fprintf(sb, " (use docker --debug to expand)")
	}
	fmt.Fprintf(sb, ":\n")
	fmt.Fprint(w, aec.Apply(sb.String(), aec.YellowF))

	for _, warn := range warnings {
		fmt.Fprintf(w, " - %s\n", warn.Short)
		if logrus.GetLevel() < logrus.DebugLevel {
			continue
		}
		for _, d := range warn.Detail {
			fmt.Fprintf(w, "%s\n", d)
		}
		if warn.URL != "" {
			fmt.Fprintf(w, "More info: %s\n", warn.URL)
		}
		if warn.SourceInfo != nil && warn.Range != nil {
			src := errdefs.Source{
				Info:   warn.SourceInfo,
				Ranges: warn.Range,
			}
			src.Print(w)
		}
		fmt.Fprintf(w, "\n")

	}
}
