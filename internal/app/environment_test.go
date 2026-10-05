package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonve/dev-cli/internal/config"
)

type overlayCapture struct {
	*lifecycleRunner
	namespace  string
	deployment []byte
}

func (r *overlayCapture) Run(ctx context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	if command == "kubectl" && len(args) > 4 && args[4] == "create" && in != nil {
		b, _ := io.ReadAll(in)
		if strings.Contains(string(b), `"kind":"Deployment"`) {
			r.namespace, r.deployment = args[3], b
		}
		in = bytes.NewReader(b)
	}
	return r.lifecycleRunner.Run(ctx, command, args, in)
}

func serveRegistry(t *testing.T, cfg *config.Config) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	cfg.Registry = strings.TrimPrefix(server.URL, "https://")
}

func TestUpAppliesEnvironmentOverridesAndConfigRelativeTemplates(t *testing.T) {
	base, cfg, _ := lifecycleFixture(t)
	base.stopOverlay = true
	cfg.Hooks, cfg.Tasks = config.Hooks{}, nil
	cfg.Namespaces = append(cfg.Namespaces, "prod-workers")
	serveRegistry(t, &cfg)
	environment := t.TempDir()
	template := "spec:\n  containers:\n    - name: app\n      env: [{name: FROM_ENVIRONMENT, value: \"yes\"}]\n"
	if err := os.WriteFile(filepath.Join(environment, "worker.yaml"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	raw["manifestsRelativeTo"] = "config"
	raw["deployables"] = map[string]any{"worker": map[string]any{"namespace": "prod-workers", "workloadTemplate": "worker.yaml"}}
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(environment, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := `{"name":"worker","kind":"worker","namespace":"dev-workers","workloadTemplate":".dev/worker.yaml",` +
		`"containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`
	if err := os.WriteFile("metadata.json", []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	r := &overlayCapture{lifecycleRunner: base}

	var out bytes.Buffer
	err := run(
		context.Background(),
		[]string{"up", "--config", filepath.Join(environment, "config.json"), "--owner", "alice", "--no-watch"},
		&out,
		&out,
		r,
	)
	if err == nil || err.Error() != "overlay boundary" {
		t.Fatalf("did not reach overlay boundary: %v %s", err, out.String())
	}
	if r.namespace != "prod-workers" || !strings.Contains(string(r.deployment), "FROM_ENVIRONMENT") {
		t.Fatalf("overlay in %q ignored the environment: %s", r.namespace, r.deployment)
	}
}
