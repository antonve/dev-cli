package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

type lifecycleRunner struct {
	root        string
	objects     map[string][]byte
	events      []string
	failTask    string
	failWait    bool
	stopOverlay bool
	expiry      string
}

func (r *lifecycleRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func (r *lifecycleRunner) Run(_ context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	joined := strings.Join(args, " ")
	if command == "git" {
		switch {
		case joined == "rev-parse --show-toplevel":
			return []byte(r.root), nil
		case joined == "branch --show-current":
			return []byte("feature/hooks"), nil
		case strings.HasPrefix(joined, "diff "):
			return []byte("change.txt"), nil
		case strings.HasPrefix(joined, "ls-files"):
			return nil, nil
		default:
			return []byte("revision"), nil
		}
	}
	if command == "bazel" {
		if args[0] == "query" {
			return []byte("//:worker"), nil
		}
		if args[0] == "cquery" {
			return []byte(filepath.Join(r.root, "metadata.json")), nil
		}
		if args[0] == "run" {
			r.events = append(r.events, "push")
		}
		return nil, nil
	}
	ns, verb := args[3], args[4]
	key := func(kind, name string) string { return ns + "/" + strings.ToLower(kind) + "/" + name }
	if verb == "get" {
		if args[5] == "deployments" || args[5] == "httproutes" || strings.Contains(args[5], ",") {
			items := []json.RawMessage{}
			for k, b := range r.objects {
				if strings.HasPrefix(k, ns+"/") && (strings.Contains(args[5], ",") || strings.Contains(k, "/"+strings.TrimSuffix(args[5], "s")+"/")) {
					items = append(items, b)
				}
			}
			return json.Marshal(map[string]any{"items": items})
		}
		if args[5] == "job" {
			if b := r.objects[key(args[5], args[6])]; len(b) > 0 {
				r.events = append(r.events, "complete:"+jobTask(b))
			}
		}
		return r.objects[key(args[5], args[6])], nil
	}
	if verb == "delete" {
		if args[5] == "configmap" {
			r.events = append(r.events, "delete-marker")
			for k := range r.objects {
				if strings.HasPrefix(k, ns+"/configmap/lifecycle-") {
					delete(r.objects, k)
				}
			}
			return nil, nil
		}
		if args[5] == "jobs" {
			r.events = append(r.events, "delete-jobs")
			return nil, nil
		}
		r.events = append(r.events, "delete-overlays")
		for k := range r.objects {
			if strings.HasPrefix(k, ns+"/") && !strings.Contains(k, "/lease/") && !strings.Contains(k, "/job/") && !strings.Contains(k, "/configmap/lifecycle-") {
				delete(r.objects, k)
			}
		}
		return nil, nil
	}
	if verb == "wait" {
		r.events = append(r.events, "wait-pods")
		if r.failWait {
			return nil, errors.New("pods remain")
		}
		return nil, nil
	}
	if verb != "create" && verb != "replace" {
		return nil, nil
	}
	b, _ := io.ReadAll(in)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	objects := []any{raw}
	if raw["kind"] == "List" {
		objects = raw["items"].([]any)
	}
	for _, v := range objects {
		obj := v.(map[string]any)
		meta := obj["metadata"].(map[string]any)
		kind := obj["kind"].(string)
		name := meta["name"].(string)
		meta["resourceVersion"] = "1"
		if kind == "Deployment" {
			r.events = append(r.events, "overlay")
			if r.stopOverlay {
				return nil, errors.New("overlay boundary")
			}
		}
		if kind == "Job" {
			task := meta["labels"].(map[string]any)[kube.ServiceLabel].(string)
			owner := meta["labels"].(map[string]any)[kube.OwnerLabel].(string)
			r.events = append(r.events, "job:"+task+":"+owner)
			condition := "Complete"
			if task == r.failTask {
				condition = "Failed"
			}
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": condition, "status": "True"}}}
		}
		r.objects[key(kind, name)], _ = json.Marshal(obj)
	}
	return nil, nil
}
func jobTask(b []byte) string {
	var o struct {
		Metadata struct{ Labels map[string]string }
	}
	_ = json.Unmarshal(b, &o)
	return o.Metadata.Labels[kube.ServiceLabel]
}

func lifecycleFixture(t *testing.T) (*lifecycleRunner, config.Config, kube.Client) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	r := &lifecycleRunner{root: root, objects: map[string][]byte{}}
	cfg := config.Config{ClusterIssuer: "lab-ca-acme", KubeContext: "dev", Namespace: "ns", Namespaces: []string{"ns"}, Registry: "registry.test", TaskLockNamespace: "ns", TTL: "8h"}
	for _, name := range []string{"before", "start", "stop", "after"} {
		if err := os.WriteFile(name+".json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"busybox"}]}}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Tasks = append(cfg.Tasks, config.Task{Name: name, Namespace: "ns", Manifest: name + ".json", Target: "db", Timeout: "5m"})
	}
	cfg.Hooks = config.Hooks{BeforeUp: []string{"before"}, AfterDown: []string{"after"}, Deployables: map[string]config.DeployableHooks{"worker": {BeforeStart: []string{"start"}, AfterStop: []string{"stop"}}}}
	return r, cfg, kube.Client{Run: r, Context: "dev", Namespace: "ns"}
}
func putMarker(t *testing.T, k kube.Client, cfg config.Config, owner, route string) {
	t.Helper()
	m := kube.Lifecycle{Owner: owner, Hooks: cfg.Hooks, Deployables: []string{"worker"}}
	if err := k.WriteLifecycle(context.Background(), m, "feature/hooks", "revision", "main", "base", route, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}
func eventIndex(events []string, want string) int {
	for i, s := range events {
		if strings.HasPrefix(s, want) {
			return i
		}
	}
	return -1
}

func TestBeforeUpFailureStopsBeforePushOrOverlay(t *testing.T) {
	r, cfg, _ := lifecycleFixture(t)
	r.failTask = "before"
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile("config.json", b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "task before failed") {
		t.Fatalf("expected hook failure: %v", err)
	}
	if eventIndex(r.events, "push") >= 0 || eventIndex(r.events, "overlay") >= 0 {
		t.Fatalf("unsafe startup: %v", r.events)
	}
	if len(r.objects["ns/configmap/"+naming.Resource("lifecycle", naming.RouteKey("alice", "feature/hooks"))]) == 0 {
		t.Fatal("failed start lost teardown marker")
	}
}
func TestBeforeStartCompletesBeforeDeploymentWrite(t *testing.T) {
	r, cfg, _ := lifecycleFixture(t)
	r.stopOverlay = true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	cfg.Registry = strings.TrimPrefix(server.URL, "https://")
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile("config.json", b, 0600)
	_ = os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600)
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
	if err == nil || err.Error() != "overlay boundary" {
		t.Fatalf("did not reach overlay boundary: %v %s", err, out.String())
	}
	complete, overlay := eventIndex(r.events, "complete:start"), eventIndex(r.events, "overlay")
	if complete < 0 || overlay <= complete {
		t.Fatalf("beforeStart ordering: %v", r.events)
	}
}
func TestTeardownWaitsThenHooksAndDeletesMarkerLast(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	r.objects["ns/deployment/worker-route"] = []byte(`{"metadata":{"labels":{"dev-cli.io/service":"worker"}}}`)
	r.events = nil
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"delete-overlays", "wait-pods"}, {"wait-pods", "job:stop"}, {"complete:stop", "job:after"}, {"complete:after", "delete-marker"}, {"delete-jobs", "delete-marker"}} {
		if a, b := eventIndex(r.events, pair[0]), eventIndex(r.events, pair[1]); a < 0 || b <= a {
			t.Fatalf("ordering %v: %v", pair, r.events)
		}
	}
	if r.events[len(r.events)-1] != "delete-marker" {
		t.Fatalf("marker not last: %v", r.events)
	}
}
func TestPodWaitFailurePreventsAfterStop(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	r.failWait = true
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err == nil {
		t.Fatal("pod wait failure ignored")
	}
	if eventIndex(r.events, "job:stop") >= 0 || eventIndex(r.events, "delete-marker") >= 0 {
		t.Fatalf("premature teardown: %v", r.events)
	}
}
func TestFailedAfterDownKeepsMarkerAndRetriesOnlyHooks(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	r.failTask = "after"
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err == nil {
		t.Fatal("hook failure ignored")
	}
	if eventIndex(r.events, "delete-marker") >= 0 {
		t.Fatal("failed teardown lost marker")
	}
	r.failTask = ""
	r.events = nil
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err != nil {
		t.Fatal(err)
	}
	if eventIndex(r.events, "delete-overlays") >= 0 || eventIndex(r.events, "wait-pods") >= 0 || eventIndex(r.events, "job:after") < 0 || r.events[len(r.events)-1] != "delete-marker" {
		t.Fatalf("retry: %v", r.events)
	}
}
func TestExpiredCleanupUsesRecordedOwnerAndHooks(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "another-owner", "route")
	cfg.Hooks = config.Hooks{}
	n, err := cleanup(context.Background(), k, cfg, time.Now(), true, io.Discard)
	if err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if eventIndex(r.events, "job:stop:another-owner") < 0 || eventIndex(r.events, "job:after:another-owner") < 0 {
		t.Fatalf("recorded owner/hooks missing: %v", r.events)
	}
}
func TestMissingRecordedTaskSkipsWithoutDeletion(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	cfg.Tasks = nil
	r.events = nil
	var warning bytes.Buffer
	n, err := cleanup(context.Background(), k, cfg, time.Now(), true, &warning)
	if err != nil || n != 0 || !strings.Contains(warning.String(), "route") || !strings.Contains(warning.String(), "after") || len(r.events) > 0 {
		t.Fatalf("unsafe missing task: %d %v %q %v", n, err, warning.String(), r.events)
	}
}
func TestStatusMaintenanceNeverRunsRecordedHooks(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	r.events = nil
	if _, err := cleanup(context.Background(), k, cfg, time.Now(), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := status(context.Background(), k, cfg, "alice", "feature/hooks", "route", false, &out); err != nil {
		t.Fatal(err)
	}
	if len(r.events) > 0 || !strings.Contains(out.String(), `"teardownPending": true`) {
		t.Fatalf("status performed teardown or omitted pending: %v %s", r.events, out.String())
	}
}
func TestRejectUnknownDeployableHook(t *testing.T) {
	_, cfg, _ := lifecycleFixture(t)
	if err := validateDeployables(cfg, []config.Deployable{{Name: "other", Kind: "worker", ContainerPath: "/app/other"}}); err == nil {
		t.Fatal("unknown deployable hook accepted")
	}
}
