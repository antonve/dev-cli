package gitx

import (
	"context"
	"io"
	"testing"
)

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	if len(args) > 0 && args[0] == "diff" {
		return []byte("apps/a/main.go\napps/b/main.go\n"), nil
	}
	if len(args) > 0 && args[0] == "ls-files" {
		return []byte("apps/b/main.go\napps/c/new.go\n"), nil
	}
	return []byte("value\n"), nil
}
func (fakeRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func TestChanged(t *testing.T) {
	v, err := (Git{Run: fakeRunner{}}).Changed(context.Background(), "base")
	if err != nil || len(v) != 3 || v[0] != "apps/a/main.go" || v[2] != "apps/c/new.go" {
		t.Fatalf("%v %v", v, err)
	}
}
