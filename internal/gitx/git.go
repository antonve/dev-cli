package gitx

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/antonve/dev-cli/internal/execx"
)

type Git struct{ Run execx.Runner }

func (g Git) one(ctx context.Context, args ...string) (string, error) {
	b, err := g.Run.Run(ctx, "git", args, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (g Git) Root(ctx context.Context) (string, error) {
	return g.one(ctx, "rev-parse", "--show-toplevel")
}
func (g Git) Branch(ctx context.Context) (string, error) {
	v, err := g.one(ctx, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("detached HEAD is not supported")
	}
	return v, nil
}
func (g Git) Revision(ctx context.Context) (string, error) { return g.one(ctx, "rev-parse", "HEAD") }
func (g Git) MergeBase(ctx context.Context, base string) (string, error) {
	return g.one(ctx, "merge-base", base, "HEAD")
}
func (g Git) Changed(ctx context.Context, base string) ([]string, error) {
	b, err := g.Run.Run(ctx, "git", []string{"diff", "--name-only", base}, nil)
	if err != nil {
		return nil, err
	}
	untracked, err := g.Run.Run(ctx, "git", []string{"ls-files", "--others", "--exclude-standard"}, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, path := range append(strings.Fields(string(b)), strings.Fields(string(untracked))...) {
		seen[path] = true
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
func (g Git) Owner(ctx context.Context) string {
	if v := execx.EnvOwner(); v != "" {
		return v
	}
	if v, err := g.one(ctx, "config", "user.email"); err == nil && v != "" {
		return v
	}
	if v, err := g.one(ctx, "config", "user.name"); err == nil && v != "" {
		return v
	}
	return "unknown"
}
