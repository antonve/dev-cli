package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

type resourceRunner struct {
	objects   map[string][]byte
	deletes   []string
	calls     []string
	failJob   bool
	activeJob bool
}

func (r *resourceRunner) key(args []string, kind, name string) string {
	return args[3] + "/" + strings.ToLower(kind) + "/" + name
}
func (r *resourceRunner) Run(_ context.Context, _ string, args []string, in io.Reader) ([]byte, error) {
	verb := args[4]
	r.calls = append(r.calls, strings.Join(args, " "))
	if verb == "apply" && strings.Contains(strings.Join(args, " "), "--dry-run=server") {
		b, _ := io.ReadAll(in)
		return b, nil
	}
	if verb == "get" {
		return r.objects[r.key(args, args[5], args[6])], nil
	}
	if verb == "delete" {
		r.deletes = append(r.deletes, strings.Join(args, " "))
		delete(r.objects, r.key(args, args[5], args[6]))
		return nil, nil
	}
	if verb == "wait" {
		return nil, nil
	}
	if verb != "apply" && verb != "create" && verb != "replace" {
		return nil, nil
	}
	b, _ := io.ReadAll(in)
	var object map[string]any
	if err := json.Unmarshal(b, &object); err != nil {
		return nil, err
	}
	objects := []any{object}
	if object["kind"] == "List" {
		objects = object["items"].([]any)
	}
	for _, value := range objects {
		item := value.(map[string]any)
		metadata := item["metadata"].(map[string]any)
		name := metadata["name"].(string)
		kind := item["kind"].(string)
		key := r.key(args, kind, name)
		if verb == "create" && r.objects[key] != nil {
			return nil, errors.New("already exists")
		}
		if kind == "Lease" {
			metadata["resourceVersion"] = "2"
		}
		if kind == "Job" {
			if r.activeJob {
				item["status"] = map[string]any{"active": 1}
			} else if r.failJob {
				item["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True"}}}
			} else {
				item["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
			}
		}
		r.objects[key], _ = json.Marshal(item)
	}
	return nil, nil
}

func TestProvisionJSONPathReadiness(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("postgres.json", `{"apiVersion":"acid.zalan.do/v1","kind":"postgresql","metadata":{"name":"db-${DEV_ROUTE}"},"spec":{}}`); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	d := config.Dependency{Name: "db", Namespace: "apps", Manifest: "postgres.json", Retention: "retain", Readiness: []config.Readiness{{Resource: "postgresql", Name: "db-${DEV_ROUTE}", JSONPath: ".status.PostgresClusterStatus", Value: "Running"}}}
	if err := c.Provision(context.Background(), d, "alice", "route-a"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, `--for=jsonpath={.status.PostgresClusterStatus}=Running`) || strings.Contains(joined, "condition=jsonpath") {
		t.Fatalf("invalid readiness command:\n%s", joined)
	}
}
func (*resourceRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func TestProvisionIsIdempotentAndCollisionSafe(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("dependency.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"db-${DEV_ROUTE}"},"data":{"endpoint":"postgres"}}`); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	d := config.Dependency{Name: "db", Namespace: "apps", Manifest: "dependency.json", Retention: "retain"}
	for range 2 {
		if err := c.Provision(context.Background(), d, "alice", "route-a"); err != nil {
			t.Fatal(err)
		}
	}
	key := "apps/configmap/db-route-a"
	if r.objects[key] == nil {
		t.Fatalf("missing %s", key)
	}
	r.objects["apps/configmap/db-route-b"] = []byte(`{"metadata":{"labels":{"app":"shared"}}}`)
	if err := c.Provision(context.Background(), d, "bob", "route-b"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("collision error = %v", err)
	}
	if err := c.RemoveDependency(context.Background(), d, "alice", "route-a"); err != nil {
		t.Fatal(err)
	}
	if r.objects[key] == nil {
		t.Fatal("retained dependency was deleted")
	}
}

func TestTaskTargetLeaseSerializesAcrossTasksAndOwners(t *testing.T) {
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	a := config.Task{Name: "migrate", Namespace: "apps", Target: "postgres.apps", Timeout: "1m"}
	release, err := c.acquireTaskLock(context.Background(), a, "locks", "apps/route-a/migrate", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var encoded struct {
		Spec struct {
			RenewTime string `json:"renewTime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(r.objects["locks/lease/"+lockName(a.Target)], &encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(microTimeFormat, encoded.Spec.RenewTime); err != nil {
		t.Fatalf("invalid Kubernetes MicroTime %q: %v", encoded.Spec.RenewTime, err)
	}
	b := a
	b.Name = "seed"
	if _, err := c.acquireTaskLock(context.Background(), b, "locks", "other/route-b/seed", time.Minute); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second lock = %v", err)
	}
}

func TestExpiredLeaseDoesNotBypassActiveJob(t *testing.T) {
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	task := config.Task{Name: "migrate", Namespace: "apps", Target: "postgres.apps", Timeout: "1m"}
	release, err := c.acquireTaskLock(context.Background(), task, "locks", "apps/route-a/migrate", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var held map[string]any
	key := "locks/lease/" + lockName(task.Target)
	if err := json.Unmarshal(r.objects[key], &held); err != nil {
		t.Fatal(err)
	}
	held["spec"].(map[string]any)["renewTime"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	r.objects[key], _ = json.Marshal(held)
	r.objects["apps/job/"+naming.Resource("migrate", "route-a")] = []byte(`{"status":{"active":1}}`)
	if _, err := c.acquireTaskLock(context.Background(), task, "locks", "apps/route-b/seed", time.Minute); err == nil {
		t.Fatal("expired lock bypassed active Job")
	}
}

func TestTaskTimeoutRetainsLockWhileJobMayRun(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("job.json", `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ignored"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"migrate","image":"migration"}]}}}}`); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}, activeJob: true}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	task := config.Task{Name: "migrate", Namespace: "apps", Manifest: "job.json", Target: "postgres.apps", Timeout: "20ms"}
	if err := c.RunTask(context.Background(), task, "locks", "", "rev", "alice", "route-a"); err == nil {
		t.Fatal("expected timeout")
	}
	var held lease
	if err := json.Unmarshal(r.objects["locks/lease/"+lockName(task.Target)], &held); err != nil {
		t.Fatal(err)
	}
	if held.Spec.Holder == "" {
		t.Fatal("active task lock was released")
	}
}

func TestFailedTaskReturnsBeforeDependentStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("job.json", `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ignored"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"migrate","image":"migration@sha256:abc"}]}}}}`); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}, failJob: true}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	task := config.Task{Name: "migrate", Namespace: "apps", Manifest: "job.json", Target: "postgres.apps", Timeout: "1s"}
	err := c.RunTask(context.Background(), task, "locks", "", "revision", "alice", "route-a")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("task error = %v", err)
	}
	if len(r.deletes) != 1 || !strings.Contains(r.deletes[0], "delete jobs -l") || !strings.Contains(r.deletes[0], ServiceLabel+"=migrate") {
		t.Fatalf("unsafe rerun cleanup: %v", r.deletes)
	}
}

func osWrite(path, contents string) error { return os.WriteFile(path, []byte(contents), 0600) }
