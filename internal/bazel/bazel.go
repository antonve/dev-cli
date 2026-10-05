package bazel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
)

type Bazel struct {
	Run  execx.Runner
	Args []string
}

func (b Bazel) args(command string, args ...string) []string {
	return append(append([]string{command}, b.Args...), args...)
}

func lines(b []byte) []string {
	var out []string
	for _, s := range strings.Fields(string(b)) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
func (b Bazel) query(ctx context.Context, expression string) ([]string, error) {
	out, err := b.Run.Run(ctx, "bazel", []string{"query", expression, "--output=label", "--noshow_progress"}, nil)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

func (b Bazel) Metadata(ctx context.Context, query string, changed []string) ([]config.Deployable, error) {
	all, err := b.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query deployables: %w", err)
	}
	selected := all
	if len(changed) > 0 {
		sources, err := b.query(ctx, `kind("source file", deps(//...))`)
		if err != nil {
			return nil, err
		}
		byPath := map[string]string{}
		for _, label := range sources {
			if p, ok := labelPath(label); ok {
				byPath[p] = label
			}
		}
		var labels []string
		unknown := false
		for _, p := range changed {
			if l, ok := byPath[filepath.ToSlash(p)]; ok {
				labels = append(labels, l)
			} else {
				unknown = true
			}
		}
		if !unknown && len(labels) > 0 {
			expr := fmt.Sprintf("rdeps(//..., set(%s)) intersect (%s)", strings.Join(labels, " "), query)
			selected, err = b.query(ctx, expr)
			if err != nil {
				return nil, fmt.Errorf("query affected deployables: %w", err)
			}
		}
	}
	if len(changed) == 0 {
		selected = nil
	}
	return b.metadataForTargets(ctx, selected)
}

func (b Bazel) AllMetadata(ctx context.Context, query string) ([]config.Deployable, error) {
	targets, err := b.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query deployables: %w", err)
	}
	return b.metadataForTargets(ctx, targets)
}

func (b Bazel) metadataForTargets(ctx context.Context, selected []string) ([]config.Deployable, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	if _, err := b.Run.Run(ctx, "bazel", append(b.args("build", "--noshow_progress"), selected...), nil); err != nil {
		return nil, err
	}
	var result []config.Deployable
	for _, target := range selected {
		out, err := b.Run.Run(ctx, "bazel", b.args("cquery", target, "--output=files", "--noshow_progress"), nil)
		if err != nil {
			return nil, err
		}
		files := lines(out)
		if len(files) != 1 {
			return nil, fmt.Errorf("metadata %s produced %d files", target, len(files))
		}
		data, err := os.ReadFile(files[0])
		if err != nil {
			return nil, err
		}
		var d config.Deployable
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("parse %s: %w", files[0], err)
		}
		d.MetadataTarget = target
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (b Bazel) PushImage(ctx context.Context, target, repository, tag string) error {
	args := b.args("run", "--noshow_progress", target, "--", "--repository", repository, "--tag", tag)
	if _, err := b.Run.Run(ctx, "bazel", args, nil); err != nil {
		return fmt.Errorf("push %s to %s:%s: %w", target, repository, tag, err)
	}
	return nil
}

func labelPath(label string) (string, bool) {
	if !strings.HasPrefix(label, "//") || strings.HasPrefix(label, "@@") {
		return "", false
	}
	v := strings.TrimPrefix(label, "//")
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] == "" {
		return parts[1], true
	}
	return parts[0] + "/" + parts[1], true
}
func (b Bazel) BuildOutput(ctx context.Context, target string) (string, error) {
	if _, err := b.Run.Run(ctx, "bazel", b.args("build", target, "--noshow_progress"), nil); err != nil {
		return "", err
	}
	// A binary reached through an OCI rule may remain in Bazel's query universe
	// in both target and transitioned configurations. Only the target
	// configuration is the executable produced by the direct build above.
	out, err := b.Run.Run(ctx, "bazel", b.args("cquery", configuredTarget(target), "--output=files", "--noshow_progress"), nil)
	if err != nil {
		return "", err
	}
	files := lines(out)
	if len(files) != 1 {
		return "", fmt.Errorf("target %s produced %d files", target, len(files))
	}
	return files[0], nil
}

func configuredTarget(target string) string { return "config(" + target + ", target)" }

// Definition returns a target's rule as `bazel query --output=build` prints it.
func (b Bazel) Definition(ctx context.Context, target string) (string, error) {
	out, err := b.Run.Run(ctx, "bazel", []string{"query", "--output=build", "--noshow_progress", target}, nil)
	return string(out), err
}
