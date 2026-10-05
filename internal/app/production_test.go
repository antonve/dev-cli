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
	"sync/atomic"
	"testing"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

// Shaped like `bazel query --output=build //services/tadoku-api:push`.
const releasePushDefinition = `# services/tadoku-api/BUILD.bazel:75:9
oci_push(
  name = "push",
  image = "//services/tadoku-api:image",
  repository = "ghcr.io/tadoku/tadoku/tadoku-api",
  remote_tags = "//services/tadoku-api:push_write_tags",
)
`

const branchPushDefinition = `# services/tadoku-api/BUILD.bazel:83:9
oci_push(
  name = "branch_push",
  image = "//services/tadoku-api:image",
)
`

type queryRunner struct{ definition string }

func (r queryRunner) Run(context.Context, string, []string, io.Reader) ([]byte, error) {
	return []byte(r.definition), nil
}
func (queryRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func TestPushGuard(t *testing.T) {
	const registry = "ghcr.io/team/repo/branches"
	const tag = "alice-feature-1a2b3c4d-0123456789ab"
	for name, tc := range map[string]struct {
		repository, tag, definition string
		allowed                     bool
	}{
		"branch target":             {registry + "/api", tag, branchPushDefinition, true},
		"latest tag":                {registry + "/api", "latest", branchPushDefinition, false},
		"prod tag":                  {registry + "/api", "prod", branchPushDefinition, false},
		"tag without commit":        {registry + "/api", "alice-feature-1a2b3c4d", branchPushDefinition, false},
		"uppercase tag":             {registry + "/api", "Alice-0123456789ab", branchPushDefinition, false},
		"release repository":        {"ghcr.io/team/repo/api", tag, branchPushDefinition, false},
		"nested escape":             {registry + "/../api", tag, branchPushDefinition, false},
		"prefix lookalike":          {registry + "-old/api", tag, branchPushDefinition, false},
		"fixed repository and tags": {registry + "/api", tag, releasePushDefinition, false},
		"fixed tags only":           {registry + "/api", tag, "oci_push(\n  remote_tags = \":tags\",\n)\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			bz := bazel.Bazel{Run: queryRunner{tc.definition}}
			err := guardPush(context.Background(), bz, registry, "//:target", tc.repository, tc.tag)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t err=%v", tc.allowed, err)
			}
		})
	}
}

type productionRunner struct {
	*lifecycleRunner
	dirty, unpushed bool
	changed         string
	namespaceLabels string
	definition      string
	pushes          []string
	published       *atomic.Bool
}

func (r *productionRunner) Run(ctx context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case command == "git" && joined == "status --porcelain":
		if r.dirty {
			return []byte(" M services/api/main.go\n"), nil
		}
		return nil, nil
	case command == "git" && joined == "fetch origin":
		return nil, nil
	case command == "git" && joined == "branch -r --contains HEAD":
		if r.unpushed {
			return nil, nil
		}
		return []byte("  origin/feature/hooks\n"), nil
	case command == "git" && joined == "rev-parse HEAD":
		return []byte("0123456789abcdef0123456789abcdef01234567"), nil
	case command == "git" && strings.HasPrefix(joined, "diff "):
		return []byte(r.changed), nil
	case command == "bazel" && strings.HasPrefix(joined, "query --output=build"):
		return []byte(r.definition), nil
	case command == "bazel" && args[0] == "run":
		r.pushes = append(r.pushes, joined)
		r.published.Store(true)
	case command == "kubectl" && len(args) > 6 && args[4] == "get" && args[5] == "namespace":
		return []byte(`{"metadata":{"labels":` + r.namespaceLabels + `}}`), nil
	}
	return r.lifecycleRunner.Run(ctx, command, args, in)
}

const productionLabels = `{"dev-cli.io/routing-enabled":"true","dev-cli.io/environment":"production"}`

// productionFixture writes a production configuration outside the checkout and
// serves a registry that knows the branch tag once published (or always).
func productionFixture(t *testing.T, tagExists bool) (*productionRunner, string, string) {
	t.Helper()
	base, _, _ := lifecycleFixture(t)
	published := &atomic.Bool{}
	published.Store(tagExists)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !published.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	registry := strings.TrimPrefix(server.URL, "https://") + "/team/branches"

	environment := t.TempDir()
	template := "spec:\n  containers:\n    - name: app\n      command: [/release-worker]\n"
	if err := os.WriteFile(filepath.Join(environment, "worker.yaml"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"mode":                "production",
		"kubeContext":         "prod",
		"namespace":           "ns",
		"registry":            registry,
		"hostTLS":             "gateway",
		"bazelArgs":           []string{"--config=release"},
		"refuseChangedPaths":  []string{"services/api/migrations/"},
		"manifestsRelativeTo": "config",
		"deployables":         map[string]any{"worker": map[string]any{"workloadTemplate": "worker.yaml"}},
		"tasks": []any{map[string]any{
			"name": "seed", "namespace": "ns", "manifest": "seed.json", "target": "db", "timeout": "5m",
		}},
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(environment, "config.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	job := `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"seed"},` +
		`"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"seed","image":"busybox"}]}}}}`
	if err := os.WriteFile(filepath.Join(environment, "seed.json"), []byte(job), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := `{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker-dev",` +
		`"pushTarget":"//:dev_push","release":{"imageName":"worker","pushTarget":"//:branch_push"}}`
	if err := os.WriteFile("metadata.json", []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	r := &productionRunner{
		lifecycleRunner: base,
		changed:         "services/api/main.go",
		namespaceLabels: productionLabels,
		definition:      branchPushDefinition,
		published:       published,
	}
	return r, path, registry
}

func TestProductionPreflightRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*productionRunner)
		command []string
		want    string
	}{
		"dirty tree":         {func(r *productionRunner) { r.dirty = true }, []string{"up"}, "not clean"},
		"unpushed HEAD":      {func(r *productionRunner) { r.unpushed = true }, []string{"up"}, "not pushed"},
		"migration change":   {func(r *productionRunner) { r.changed = "services/api/migrations/0001.up.sql" }, []string{"up"}, "services/api/migrations/"},
		"unlabelled routing": {func(r *productionRunner) { r.namespaceLabels = `{"dev-cli.io/routing-enabled":"true"}` }, []string{"up"}, "dev-cli.io/environment=production"},
		"dirty task":         {func(r *productionRunner) { r.dirty = true }, []string{"task", "seed"}, "not clean"},
		"dirty provision":    {func(r *productionRunner) { r.dirty = true }, []string{"provision", "nothing"}, "not clean"},
		"release push target": {
			func(r *productionRunner) { r.definition = releasePushDefinition },
			[]string{"up"},
			"repository or remote_tags",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, path, _ := productionFixture(t, false)
			tc.mutate(r)
			var out bytes.Buffer
			args := append(append([]string{}, tc.command[0], "--config", path, "--owner", "alice"), tc.command[1:]...)
			err := run(context.Background(), args, &out, &out, r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected refusal containing %q: %v %s", tc.want, err, out.String())
			}
			if len(r.pushes) > 0 || eventIndex(r.events, "overlay") >= 0 || eventIndex(r.events, "job:") >= 0 {
				t.Fatalf("refusal happened after mutation: pushes=%v events=%v", r.pushes, r.events)
			}
		})
	}
}

func TestDevelopmentRefusesProductionRoutingNamespace(t *testing.T) {
	base, cfg, _ := lifecycleFixture(t)
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile("config.json", b, 0600); err != nil {
		t.Fatal(err)
	}
	r := &productionRunner{lifecycleRunner: base, namespaceLabels: productionLabels, published: &atomic.Bool{}}
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "production") {
		t.Fatalf("development mode used a production namespace: %v", err)
	}
	if eventIndex(r.events, "job:") >= 0 || len(r.pushes) > 0 {
		t.Fatalf("refusal happened after mutation: %v", r.events)
	}
}

func TestProductionUpReusesAnExistingCommitTag(t *testing.T) {
	r, path, registry := productionFixture(t, true)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"up", "--config", path, "--owner", "alice"}, &out, &out, r); err != nil {
		t.Fatalf("production up: %v %s", err, out.String())
	}
	if len(r.pushes) != 0 {
		t.Fatalf("existing tag was rebuilt: %v", r.pushes)
	}
	route := naming.RouteKey("alice", "feature/hooks")
	assertReleaseOverlay(t, r, route, registry+"/worker@sha256:abc")
	if !strings.Contains(out.String(), "expires=") || !strings.Contains(out.String(), "commit=0123456789abcdef") {
		t.Fatalf("summary lacks commit or expiry: %s", out.String())
	}
}

func TestProductionUpPublishesThroughTheReleaseTarget(t *testing.T) {
	r, path, registry := productionFixture(t, false)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"up", "--config", path, "--owner", "alice"}, &out, &out, r); err != nil {
		t.Fatalf("production up: %v %s", err, out.String())
	}
	route := naming.RouteKey("alice", "feature/hooks")
	want := "run --config=release --noshow_progress //:branch_push -- --repository " + registry + "/worker --tag " + route + "-0123456789ab"
	if len(r.pushes) != 1 || r.pushes[0] != want {
		t.Fatalf("pushes:\n got %v\nwant %s", r.pushes, want)
	}
	assertReleaseOverlay(t, r, route, registry+"/worker@sha256:abc")
}

func assertReleaseOverlay(t *testing.T, r *productionRunner, route, image string) {
	t.Helper()
	deployment := string(r.objects["ns/deployment/"+naming.Resource("worker", route)])
	if !strings.Contains(deployment, `"image":"`+image+`"`) || !strings.Contains(deployment, `"command":["/release-worker"]`) {
		t.Fatalf("overlay does not run the release image unchanged: %s", deployment)
	}
	for _, development := range []string{"supervise", "DEV_BASE_BINARY", "DEV_INTERNAL_GATEWAY", `"/work"`} {
		if strings.Contains(deployment, development) {
			t.Fatalf("overlay contains development runtime %q: %s", development, deployment)
		}
	}
	if len(r.objects["ns/configmap/"+naming.Resource("worker", route)+"-runtime"]) > 0 {
		t.Fatal("production overlay created the supervisor ConfigMap")
	}
}

func TestProductionDeployablesFailClosed(t *testing.T) {
	cfg := config.Config{
		Mode:        config.ModeProduction,
		Namespace:   "ns",
		Namespaces:  []string{"ns"},
		Deployables: map[string]config.DeployableOverride{"worker": {WorkloadTemplate: "worker.yaml"}},
	}
	release := config.Release{ImageName: "worker", PushTarget: "//:branch_push"}
	worker := config.Deployable{Name: "worker", Kind: "worker", ContainerPath: "/worker", WorkloadTemplate: "worker.yaml", Release: release}
	if err := validateDeployables(cfg, []config.Deployable{worker}); err != nil {
		t.Fatalf("valid production deployable: %v", err)
	}

	withoutRelease := worker
	withoutRelease.Release = config.Release{}
	if err := validateDeployables(cfg, []config.Deployable{withoutRelease}); err == nil {
		t.Fatal("production accepted a deployable without a release entrypoint")
	}

	// A metadata template alone would run a development template in production.
	cfg.Deployables = nil
	if err := validateDeployables(cfg, []config.Deployable{worker}); err == nil {
		t.Fatal("production accepted a deployable without a workloadTemplate override")
	}
}
