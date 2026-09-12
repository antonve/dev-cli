package app

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonve/dev-cli/internal/naming"
)

func TestURLCommandNeedsOnlyGitAndConfig(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root, bin := t.TempDir(), t.TempDir()
	t.Chdir(root)
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	// No bazel, kubectl or curl is available to the command.
	t.Setenv("PATH", bin)
	for _, args := range [][]string{{"init", "-q"}, {"symbolic-ref", "HEAD", "refs/heads/feature/demo"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	if err := os.Mkdir(".dev", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".dev/config.json", []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test","ingressHost":"app.dev.lab"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, clear := range []bool{false, true} {
		args := []string{"url", "--owner", "alice"}
		want := naming.RouteKey("alice", "feature/demo")
		if clear {
			args = append(args, "--clear")
			want = "base"
		}
		args = append(args, "/settings?tab=profile#details")
		var out, stderr bytes.Buffer
		if err := Run(context.Background(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(strings.TrimSpace(out.String()))
		if err != nil || u.Query().Get("dev-branch") != want || u.Path != "/settings" || u.Fragment != "details" {
			t.Fatalf("wrong URL: %s", out.String())
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"url", "--owner", "alice", "https://evil.test/"}, &out, &out); err == nil {
		t.Fatal("accepted external destination")
	}
}
