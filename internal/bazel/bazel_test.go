package bazel

import (
	"context"
	"io"
	"strings"
	"testing"
)

type argsRunner struct{ calls []string }

func (r *argsRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if args[0] == "query" {
		return []byte("//app:meta"), nil
	}
	if args[0] == "cquery" {
		return []byte("out"), nil
	}
	return nil, nil
}
func (*argsRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func TestLabelPath(t *testing.T) {
	tests := map[string]string{"//apps/api:main.go": "apps/api/main.go", "//:MODULE.bazel": "MODULE.bazel"}
	for in, want := range tests {
		got, ok := labelPath(in)
		if !ok || got != want {
			t.Fatalf("%s = %q,%v", in, got, ok)
		}
	}
}

func TestConfiguredTarget(t *testing.T) {
	if got, want := configuredTarget("//apps/api:api"), "config(//apps/api:api, target)"; got != want {
		t.Fatalf("configuredTarget() = %q, want %q", got, want)
	}
}

func TestBazelArgsApplyToConfiguredBuildsButNotGraphQueries(t *testing.T) {
	r := &argsRunner{}
	b := Bazel{Run: r, Args: []string{"--platforms=//platforms:linux_amd64"}}
	if _, err := b.query(context.Background(), "//..."); err != nil {
		t.Fatal(err)
	}
	if _, err := b.BuildOutput(context.Background(), "//app:binary"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.calls[0], "--platforms") || !strings.Contains(r.calls[1], "--platforms=//platforms:linux_amd64") || !strings.Contains(r.calls[2], "--platforms=//platforms:linux_amd64") {
		t.Fatalf("bazel args calls: %v", r.calls)
	}
}
