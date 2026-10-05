package kube

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
)

type rolloutRunner struct {
	captureRunner
	rollouts []string
}

func (r *rolloutRunner) Run(ctx context.Context, name string, args []string, in io.Reader) ([]byte, error) {
	if args[4] == "rollout" {
		r.rollouts = append(r.rollouts, strings.Join(args[4:], " "))
	}
	return r.captureRunner.Run(ctx, name, args, in)
}

func TestProductionOverlayKeepsTheReleaseRuntime(t *testing.T) {
	for _, kind := range []string{"backend", "worker", "frontend"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			template := `spec:
  serviceAccountName: tadoku-api
  securityContext: {runAsNonRoot: true}
  containers:
    - name: app
      image: ghcr.io/team/app:prod
      command: [/release]
      args: [serve]
      env: [{name: API_BRANCH, value: "${DEV_ROUTE}"}]
      securityContext: {readOnlyRootFilesystem: true}
      readinessProbe: {httpGet: {path: /readyz, port: 8000}}
      resources: {requests: {memory: 64Mi}}
`
			if err := os.WriteFile(filepath.Join(root, "app.yaml"), []byte(template), 0600); err != nil {
				t.Fatal(err)
			}
			d := config.Deployable{
				Name:             "app",
				Kind:             kind,
				Port:             8000,
				ReadinessPath:    "/readyz",
				ContainerPath:    "/app",
				WorkloadTemplate: "app.yaml",
				DevCommand:       []string{"pnpm", "dev"},
			}
			cfg := config.Config{Namespace: "ns", Mode: config.ModeProduction}
			r := &rolloutRunner{}
			c := Client{Run: r, Context: "prod", Namespace: "ns", ManifestRoot: root}
			image := "ghcr.io/team/branches/app@sha256:abc"
			err := c.ApplyOverlay(
				context.Background(),
				cfg,
				d,
				image,
				"alice",
				"feature",
				"0123456789abcdef",
				"origin/main",
				"base",
				"alice-feature-12345678",
				time.Now().Add(time.Hour),
			)
			if err != nil {
				t.Fatal(err)
			}

			var list struct{ Items []map[string]any }
			if err := json.Unmarshal(r.payload, &list); err != nil {
				t.Fatal(err)
			}
			for _, item := range list.Items {
				if item["kind"] == "ConfigMap" {
					t.Fatal("production overlay created the development supervisor ConfigMap")
				}
			}
			deployment := list.Items[0]
			podSpec := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			container := podSpec["containers"].([]any)[0].(map[string]any)
			want := map[string]any{
				"name":            "app",
				"image":           image,
				"imagePullPolicy": "IfNotPresent",
				"command":         []any{"/release"},
				"args":            []any{"serve"},
				"env":             []any{map[string]any{"name": "API_BRANCH", "value": "alice-feature-12345678"}},
				"securityContext": map[string]any{"readOnlyRootFilesystem": true},
				"readinessProbe":  map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": float64(8000)}},
				"resources":       map[string]any{"requests": map[string]any{"memory": "64Mi"}},
			}
			if !reflect.DeepEqual(container, want) {
				t.Fatalf("release container changed:\n got %#v\nwant %#v", container, want)
			}
			if _, ok := podSpec["volumes"]; ok {
				t.Fatalf("production overlay added volumes: %#v", podSpec["volumes"])
			}
			if !reflect.DeepEqual(podSpec["securityContext"], map[string]any{"runAsNonRoot": true}) {
				t.Fatalf("pod securityContext changed: %#v", podSpec["securityContext"])
			}
			if len(r.rollouts) != 1 || !strings.HasPrefix(r.rollouts[0], "rollout status deployment/") {
				t.Fatalf("production overlay must wait for rollout: %v", r.rollouts)
			}
		})
	}
}

func TestProductionOverlayRequiresTemplate(t *testing.T) {
	d := config.Deployable{Name: "app", Kind: "backend", Port: 8000, ReadinessPath: "/readyz"}
	c := Client{Run: &captureRunner{}, Context: "prod", Namespace: "ns"}
	err := c.ApplyOverlay(
		context.Background(),
		config.Config{Namespace: "ns", Mode: config.ModeProduction},
		d,
		"image@sha256:abc",
		"alice",
		"feature",
		"rev",
		"origin/main",
		"base",
		"route",
		time.Now().Add(time.Hour),
	)
	if err == nil || !strings.Contains(err.Error(), "workload template") {
		t.Fatalf("production overlay without template: %v", err)
	}
}
