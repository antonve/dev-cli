package bazel

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// execRootRunner mimics a workspace built with --symlink_prefix=/: cquery
// prints execution-root-relative paths and no bazel-out link exists.
type execRootRunner struct {
	execRoot string
	info     int
}

func (r *execRootRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	switch args[0] {
	case "query":
		return []byte("//app:meta"), nil
	case "cquery":
		if strings.HasPrefix(args[1], "config(") {
			return []byte("bazel-out/k8-fastbuild/bin/app/binary\n"), nil
		}
		return []byte("bazel-out/k8-fastbuild/bin/app/meta.json\n"), nil
	case "info":
		r.info++
		return []byte(r.execRoot + "\n"), nil
	}
	return nil, nil
}
func (*execRootRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func TestOutputPathsResolveAgainstExecutionRootWithoutConvenienceSymlinks(t *testing.T) {
	execRoot := t.TempDir()
	bin := filepath.Join(execRoot, "bazel-out", "k8-fastbuild", "bin", "app")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "meta.json"), []byte(`{"name":"api"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // workspace without a bazel-out link
	b := Bazel{Run: &execRootRunner{execRoot: execRoot}}
	ds, err := b.AllMetadata(context.Background(), "//...")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(ds) != 1 || ds[0].Name != "api" {
		t.Fatalf("metadata = %+v", ds)
	}
	out, err := b.BuildOutput(context.Background(), "//app:binary")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(bin, "binary"); out != want {
		t.Fatalf("BuildOutput = %q, want %q", out, want)
	}
}

func TestExecutionRootIsQueriedOncePerRunAcrossCopies(t *testing.T) {
	r := &execRootRunner{execRoot: "/exec"}
	b := New(r, nil)
	loop := b // callers such as the sync loop hold copies
	for range 3 {
		if _, err := loop.BuildOutput(context.Background(), "//app:binary"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.BuildOutput(context.Background(), "//app:binary"); err != nil {
		t.Fatal(err)
	}
	if r.info != 1 {
		t.Fatalf("bazel info ran %d times", r.info)
	}
}

func TestAbsoluteOutputPathsAreUnchanged(t *testing.T) {
	r := &argsRunner{}
	b := New(r, nil)
	out, err := b.outputPath(context.Background(), "/abs/out")
	if err != nil || out != "/abs/out" || len(r.calls) != 0 {
		t.Fatalf("outputPath = %q, %v, calls %v", out, err, r.calls)
	}
}
