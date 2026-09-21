package app

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

type logsRunner struct {
	mu      sync.Mutex
	streams []string
}

func (r *logsRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	joined := strings.Join(args, " ")
	namespace := args[3]
	if strings.Contains(joined, "get deployments") {
		service, container := "api", "server"
		if namespace == "web" {
			service, container = "web", "next"
		}
		return []byte(`{"items":[{"metadata":{"labels":{"dev-cli.io/service":"` + service + `"},"annotations":{"dev-cli.io/dev-container":"` + container + `"}}}]}`), nil
	}
	return nil, nil
}
func (r *logsRunner) Stream(_ context.Context, _ string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streams = append(r.streams, strings.Join(args, " "))
	return nil
}

func TestLogsSupportsAggregateAndServiceFilterWithoutBazel(t *testing.T) {
	for _, service := range []string{"", "api"} {
		r := &logsRunner{}
		k := kube.Client{Run: r, Context: "dev", Namespace: "routes"}
		cfg := config.Config{Namespaces: []string{"api", "web"}}
		if err := logs(context.Background(), k, cfg, "route", service, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if service == "" && (len(r.streams) != 2 || !strings.Contains(strings.Join(r.streams, "\n"), "-c server") || !strings.Contains(strings.Join(r.streams, "\n"), "-c next")) {
			t.Fatalf("aggregate logs: %v", r.streams)
		}
		if service == "api" && (len(r.streams) != 1 || !strings.Contains(r.streams[0], "--namespace api")) {
			t.Fatalf("filtered logs: %v", r.streams)
		}
	}
}

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
	if err := os.WriteFile(".dev/config.yaml", []byte("kubeContext: dev\nnamespace: ns\nregistry: registry.test\ningressHost: yaml.dev.lab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{true, false} {
		args := []string{"url", "--owner", "alice"}
		if explicit {
			args = append(args, "--config", ".dev/config.yaml")
		} else if err := os.Remove(".dev/config.json"); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := Run(context.Background(), args, &out, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), "https://yaml.dev.lab/") {
			t.Fatalf("YAML command selected wrong config: %s", out.String())
		}
	}
}
