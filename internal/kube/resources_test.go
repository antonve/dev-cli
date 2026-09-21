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
)

type resourceRunner struct {
	objects map[string][]byte
	deletes []string
	failJob bool
}

func (r *resourceRunner) key(args []string, kind, name string) string {
	return args[3] + "/" + strings.ToLower(kind) + "/" + name
}
func (r *resourceRunner) Run(_ context.Context, _ string, args []string, in io.Reader) ([]byte, error) {
	verb := args[4]
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
			if r.failJob {
				item["status"] = map[string]any{"failed": 1}
			} else {
				item["status"] = map[string]any{"succeeded": 1}
			}
		}
		r.objects[key], _ = json.Marshal(item)
	}
	return nil, nil
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
	release, err := c.acquireTaskLock(context.Background(), a, "route-a/migrate", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b := a
	b.Name = "seed"
	if _, err := c.acquireTaskLock(context.Background(), b, "route-b/seed", time.Minute); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second lock = %v", err)
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
	err := c.RunTask(context.Background(), task, "", "revision", "alice", "route-a")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("task error = %v", err)
	}
	if len(r.deletes) != 1 || !strings.Contains(r.deletes[0], "job migrate-dev-route-a") {
		t.Fatalf("unsafe rerun cleanup: %v", r.deletes)
	}
}

func osWrite(path, contents string) error { return os.WriteFile(path, []byte(contents), 0600) }
