package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

// headlessRunner records kubectl calls and fails any git or Bazel call once
// the checkout is gone, proving headless teardown needs only the cluster.
type headlessRunner struct {
	*lifecycleRunner
	checkoutGone bool
	kubectl      []string
}

func (r *headlessRunner) Run(ctx context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	if r.checkoutGone && command != "kubectl" {
		return nil, &unexpectedCommand{command + " " + strings.Join(args, " ")}
	}
	if command == "kubectl" {
		r.kubectl = append(r.kubectl, strings.Join(args, " "))
		// The base fake deletes by namespace; scope deletes to the selected route.
		if route := selectedRoute(args); args[4] == "delete" && route != "" {
			hidden := map[string][]byte{}
			for key, b := range r.objects {
				if !strings.Contains(key, route) {
					hidden[key] = b
					delete(r.objects, key)
				}
			}
			defer func() { maps.Copy(r.objects, hidden) }()
		}
	}
	return r.lifecycleRunner.Run(ctx, command, args, in)
}

func selectedRoute(args []string) string {
	for i, arg := range args {
		if arg == "-l" && i+1 < len(args) {
			for _, term := range strings.Split(args[i+1], ",") {
				if value, ok := strings.CutPrefix(term, kube.RouteLabel+"="); ok {
					return value
				}
			}
		}
	}
	return ""
}

type unexpectedCommand struct{ command string }

func (e *unexpectedCommand) Error() string { return "headless teardown ran " + e.command }

// headlessFixture is a checkout whose afterDown task publishes its own image
// and whose manifests need variable substitution, plus a retention: down
// dependency. upToOverlay brings a route up to its first overlay write.
func headlessFixture(t *testing.T) (*headlessRunner, config.Config) {
	t.Helper()
	base, cfg, _ := lifecycleFixture(t)
	base.stopOverlay = true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(server.Close)
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	cfg.Registry = strings.TrimPrefix(server.URL, "https://")
	cfg.Variables = map[string]string{"TENANT": "tenant-${DEV_ROUTE}"}
	for i := range cfg.Tasks {
		if cfg.Tasks[i].Name == "after" {
			cfg.Tasks[i].ImageName, cfg.Tasks[i].PushTarget, cfg.Tasks[i].Container = "hook", "//:push_hook", "hook"
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("after.json", `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"placeholder","env":[{"name":"TENANT","value":"${DEV_VAR_TENANT}"}]}]}}}}`)
	write("db.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"db-${DEV_ROUTE}"},"data":{"tenant":"${DEV_VAR_TENANT}"}}`)
	cfg.Dependencies = []config.Dependency{{Name: "db", Namespace: "ns", Manifest: "db.json", Retention: "down"}, {Name: "kept", Namespace: "ns", Manifest: "db.json", Retention: "retain"}}
	write("metadata.json", `{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`)
	b, _ := json.Marshal(cfg)
	write("config.json", string(b))
	return &headlessRunner{lifecycleRunner: base}, cfg
}

func upToOverlay(t *testing.T, r *headlessRunner, owner string) string {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", owner, "--no-watch"}, &out, &out, r)
	if err == nil || err.Error() != "overlay boundary" {
		t.Fatalf("up did not reach the overlay boundary: %v\n%s", err, out.String())
	}
	route := naming.RouteKey(owner, "feature/hooks")
	// The overlay write was refused by the fake; put the running overlay in place.
	r.objects["ns/deployment/worker-"+route] = []byte(`{"metadata":{"name":"worker-` + route + `","labels":{"dev-cli.io/service":"worker","dev-cli.io/route":"` + route + `","dev-cli.io/managed-by":"dev-cli"},"annotations":{"dev-cli.io/expires-at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}}}`)
	return route
}

// leaveCheckout simulates an orphaned route: the checkout and its .dev config
// are gone and teardown runs from somewhere else.
func leaveCheckout(t *testing.T, r *headlessRunner) {
	t.Helper()
	t.Chdir(t.TempDir())
	r.checkoutGone, r.events, r.kubectl = true, nil, nil
}

func jobImage(r *headlessRunner, task string) string {
	for key, b := range r.objects {
		if strings.Contains(key, "/job/") && jobTask(b) == task {
			var job struct {
				Spec struct {
					Template struct {
						Spec struct{ Containers []struct{ Image string } }
					}
				}
			}
			_ = json.Unmarshal(b, &job)
			return job.Spec.Template.Spec.Containers[0].Image
		}
	}
	return ""
}

func markerKey(route string) string { return "ns/configmap/" + naming.Resource("lifecycle", route) }

func editMarker(t *testing.T, r *headlessRunner, route string, edit func(map[string]any)) {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(r.objects[markerKey(route)], &object); err != nil {
		t.Fatal(err)
	}
	edit(object)
	r.objects[markerKey(route)], _ = json.Marshal(object)
}

func expire(t *testing.T, r *headlessRunner, route string) {
	t.Helper()
	editMarker(t, r, route, func(o map[string]any) {
		o["metadata"].(map[string]any)["annotations"].(map[string]any)["dev-cli.io/expires-at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	})
	for key := range r.objects {
		if strings.Contains(key, route) && key != markerKey(route) {
			delete(r.objects, key)
		}
	}
}

func headless(r *headlessRunner, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(context.Background(), args, &out, &out, r)
	return out.String(), err
}

func assertOrder(t *testing.T, events []string, pairs ...[2]string) {
	t.Helper()
	for _, pair := range pairs {
		if a, b := eventIndex(events, pair[0]), eventIndex(events, pair[1]); a < 0 || b <= a {
			t.Fatalf("ordering %v: %v", pair, events)
		}
	}
}

func TestUpRecordsSelfSufficientTeardownInMarker(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	m, err := (kube.Client{Run: r, Context: "dev", Namespace: "ns"}).ReadLifecycle(context.Background(), route)
	if err != nil || m == nil || m.Teardown == nil {
		t.Fatalf("marker has no teardown record: %+v %v", m, err)
	}
	td := m.Teardown
	if strings.Join(td.Namespaces, ",") != "ns" || td.TaskLockNamespace != "ns" {
		t.Fatalf("namespaces not recorded: %+v", td)
	}
	if _, ok := td.Tasks["before"]; ok {
		t.Fatal("startup task recorded as teardown task")
	}
	stop, after := td.Tasks["stop"], td.Tasks["after"]
	if stop.Job == nil || stop.Namespace != "ns" || stop.Target != "db" || stop.Timeout != "5m" {
		t.Fatalf("afterStop task not recorded: %+v", stop)
	}
	job, _ := json.Marshal(after.Job)
	if !strings.Contains(string(job), `"tenant-`+route+`"`) || strings.Contains(string(job), "${") {
		t.Fatalf("afterDown Job not resolved: %s", job)
	}
	if !strings.Contains(after.Image, "/hook@sha256:") || !strings.Contains(string(job), `"image":"`+after.Image+`"`) {
		t.Fatalf("afterDown image digest not injected: %s %q", job, after.Image)
	}
	if len(td.Dependencies) != 1 || td.Dependencies[0].Name != "db" || td.Dependencies[0].Retention != "down" {
		t.Fatalf("retention: down dependency not recorded: %+v", td.Dependencies)
	}
	objects, _ := json.Marshal(td.Dependencies[0].Objects)
	if !strings.Contains(string(objects), "db-"+route) || !strings.Contains(string(objects), "tenant-"+route) {
		t.Fatalf("dependency objects not resolved: %s", objects)
	}
	if eventIndex(r.events, "push") < 0 {
		t.Fatalf("teardown image not published at up: %v", r.events)
	}
}

func TestHeadlessDownTearsDownFromMarkerAlone(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	leaveCheckout(t, r)
	out, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", route)
	if err != nil {
		t.Fatalf("headless down: %v\n%s", err, out)
	}
	assertOrder(t, r.events, [2]string{"delete-overlays", "wait-pods"}, [2]string{"wait-pods", "job:stop:alice"}, [2]string{"complete:stop", "job:after:alice"}, [2]string{"complete:after", "delete-jobs"}, [2]string{"delete-jobs", "delete-marker"})
	if r.events[len(r.events)-1] != "delete-marker" {
		t.Fatalf("marker not deleted last: %v", r.events)
	}
	if len(r.objects[markerKey(route)]) > 0 {
		t.Fatal("marker survived")
	}
	if image := jobImage(r, "after"); !strings.Contains(image, "/hook@sha256:") {
		t.Fatalf("afterDown Job ran without its recorded image: %q", image)
	}
	if !strings.Contains(strings.Join(r.kubectl, "\n"), "get configmap db-"+route) {
		t.Fatalf("retention: down dependency was not removed: %v", r.kubectl)
	}
	if strings.Contains(strings.Join(r.kubectl, "\n"), "--context dev") {
		t.Fatalf("headless down used the checkout's context: %v", r.kubectl)
	}
}

func TestHeadlessDownUsesGivenContext(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	leaveCheckout(t, r)
	if out, err := headless(r, "down", "--headless", "--context", "dev", "--namespace", "ns", "--route", route); err != nil {
		t.Fatalf("headless down: %v\n%s", err, out)
	}
	for _, call := range r.kubectl {
		if !strings.HasPrefix(call, "--context dev ") {
			t.Fatalf("call ignored --context: %s", call)
		}
	}
}

func TestHeadlessDownRefusesOlderMarkerUntouched(t *testing.T) {
	r, cfg := headlessFixture(t)
	k := kube.Client{Run: r, Context: "dev", Namespace: "ns"}
	putMarker(t, k, cfg, "alice", "route")
	before := string(r.objects[markerKey("route")])
	leaveCheckout(t, r)
	_, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", "route")
	if err == nil || !strings.Contains(err.Error(), "older dev CLI") {
		t.Fatalf("older marker not refused: %v", err)
	}
	if len(r.events) > 0 || string(r.objects[markerKey("route")]) != before {
		t.Fatalf("older marker touched: %v", r.events)
	}
	for _, call := range r.kubectl {
		if !strings.Contains(call, " get ") {
			t.Fatalf("refused route was mutated: %s", call)
		}
	}
}

func TestHeadlessDownKeepsOwnershipChecks(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	leaveCheckout(t, r)
	editMarker(t, r, route, func(o map[string]any) {
		o["metadata"].(map[string]any)["annotations"].(map[string]any)["dev-cli.io/owner-original"] = "mallory"
	})
	if _, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", route); err == nil {
		t.Fatal("marker whose labels do not match its recorded owner was accepted")
	}
	if len(r.events) > 0 {
		t.Fatalf("mismatched marker touched the route: %v", r.events)
	}
	editMarker(t, r, route, func(o map[string]any) {
		o["metadata"].(map[string]any)["annotations"].(map[string]any)["dev-cli.io/owner-original"] = "alice"
	})
	if out, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", route); err != nil {
		t.Fatalf("headless down: %v\n%s", err, out)
	}
	owner := kube.OwnerLabel + "=" + naming.Slug("alice", 40)
	for _, call := range r.kubectl {
		if strings.Contains(call, " delete ") && !strings.Contains(call, "--raw") && !strings.Contains(call, owner) {
			t.Fatalf("delete not scoped to the recorded owner: %s", call)
		}
	}
	if _, err := headless(r, "down", "--headless", "--owner", "bob", "--namespace", "ns", "--route", route); err == nil {
		t.Fatal("headless down accepted an owner override")
	}
}

func TestHeadlessRetryAfterFailedHookResumes(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	leaveCheckout(t, r)
	r.failTask = "after"
	if _, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", route); err == nil {
		t.Fatal("hook failure ignored")
	}
	if eventIndex(r.events, "delete-marker") >= 0 || len(r.objects[markerKey(route)]) == 0 {
		t.Fatalf("failed teardown lost its marker: %v", r.events)
	}
	r.failTask, r.events = "", nil
	if out, err := headless(r, "down", "--headless", "--namespace", "ns", "--route", route); err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if eventIndex(r.events, "delete-overlays") >= 0 || eventIndex(r.events, "wait-pods") >= 0 {
		t.Fatalf("retry deleted overlays twice: %v", r.events)
	}
	assertOrder(t, r.events, [2]string{"job:stop", "job:after"}, [2]string{"job:after", "delete-marker"})
	if r.events[len(r.events)-1] != "delete-marker" {
		t.Fatalf("retry: %v", r.events)
	}
}

func TestHeadlessCleanupRemovesOnlyExpiredRoutes(t *testing.T) {
	r, cfg := headlessFixture(t)
	expired := upToOverlay(t, r, "alice")
	live := upToOverlay(t, r, "bob")
	putMarker(t, kube.Client{Run: r, Context: "dev", Namespace: "ns"}, cfg, "carol", "old-route")
	oldMarker := string(r.objects[markerKey("old-route")])
	expire(t, r, expired)
	leaveCheckout(t, r)
	out, err := headless(r, "cleanup", "--headless", "--namespace", "ns")
	if err != nil {
		t.Fatalf("headless cleanup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed 1 expired route(s)") || !strings.Contains(out, "old-route") {
		t.Fatalf("cleanup output: %s", out)
	}
	if len(r.objects[markerKey(expired)]) > 0 || eventIndex(r.events, "job:after:alice") < 0 {
		t.Fatalf("expired route not torn down: %v", r.events)
	}
	if len(r.objects[markerKey(live)]) == 0 || eventIndex(r.events, "job:after:bob") >= 0 {
		t.Fatalf("live route torn down: %v", r.events)
	}
	if string(r.objects[markerKey("old-route")]) != oldMarker || eventIndex(r.events, "job:stop:carol") >= 0 {
		t.Fatal("older marker was not left untouched")
	}
}

func TestCheckoutDownStillRunsHooksFromConfig(t *testing.T) {
	r, _ := headlessFixture(t)
	route := upToOverlay(t, r, "alice")
	r.events = nil
	out, err := headless(r, "down", "--config", "config.json", "--owner", "alice")
	if err != nil {
		t.Fatalf("down: %v\n%s", err, out)
	}
	if eventIndex(r.events, "push") < 0 || eventIndex(r.events, "job:after:alice") < 0 || r.events[len(r.events)-1] != "delete-marker" || len(r.objects[markerKey(route)]) > 0 {
		t.Fatalf("checkout down changed: %v", r.events)
	}
}

func TestOversizedMarkerIsRefused(t *testing.T) {
	r, _ := headlessFixture(t)
	big := strings.Repeat("x", 1<<20)
	if err := os.WriteFile("stop.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook","annotations":{"padding":"`+big+`"}},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"busybox"}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "lifecycle marker") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized marker not refused: %v", err)
	}
	if eventIndex(r.events, "overlay") >= 0 {
		t.Fatalf("startup continued past an unrecordable teardown: %v", r.events)
	}
}
