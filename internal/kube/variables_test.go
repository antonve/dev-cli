package kube

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/antonve/dev-cli/internal/config"
)

func TestVariablesRenderWorkloadTaskAndDependency(t *testing.T) {
	t.Chdir(t.TempDir())
	vars := map[string]string{"DATABASE": "tadoku-route"}
	workload := `{"spec":{"containers":[{"name":"app","image":"original","env":[{"name":"DATABASE","value":"${DEV_VAR_DATABASE}"}]}]}}`
	task := `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"task"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"app","image":"original","env":[{"name":"DATABASE","value":"${DEV_VAR_DATABASE}"}]}]}}}}`
	dependency := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"database-${DEV_ROUTE}"},"data":{"database":"${DEV_VAR_DATABASE}","namespace":"${DEV_NAMESPACE}"}}`
	for name, data := range map[string]string{"workload.json": workload, "task.json": task, "dependency.json": dependency} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	template, err := loadPodTemplate("", "workload.json", "route", "apps", vars)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ := json.Marshal(template)
	if !strings.Contains(string(rendered), "tadoku-route") || strings.Contains(string(rendered), "DEV_VAR") {
		t.Fatalf("workload: %s", rendered)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps", Variables: vars}
	if err := c.RunTask(context.Background(), config.Task{Name: "task", Namespace: "apps", Target: "db", Manifest: "task.json", Timeout: "5m"}, "apps", "", "rev", "alice", "route"); err != nil {
		t.Fatal(err)
	}
	if err := c.Provision(context.Background(), config.Dependency{Name: "database", Namespace: "apps", Manifest: "dependency.json", Retention: "retain"}, "alice", "route"); err != nil {
		t.Fatal(err)
	}
	jobFound, dependencyFound := false, false
	for key, data := range r.objects {
		if strings.Contains(key, "/job/") {
			jobFound = strings.Contains(string(data), "tadoku-route")
		}
		if strings.Contains(key, "/configmap/database-route") {
			dependencyFound = strings.Contains(string(data), "tadoku-route")
		}
	}
	if !jobFound || !dependencyFound {
		t.Fatalf("task=%t dependency=%t", jobFound, dependencyFound)
	}
}
func TestUnknownManifestVariableRejectedBeforeMutation(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("task.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"task"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"${DEV_VAR_UNKNOWN}"}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps", Variables: map[string]string{"DATABASE": "db"}}
	err := c.RunTask(context.Background(), config.Task{Name: "task", Namespace: "apps", Target: "db", Manifest: "task.json", Timeout: "5m"}, "apps", "", "rev", "alice", "route")
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") || len(r.calls) > 0 {
		t.Fatalf("unknown variable reached mutation: %v %v", err, r.calls)
	}
}

func TestTaskTargetResolvesPerRouteAndSerializesLifecycleTasks(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("task.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"task"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"app","image":"busybox"}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps", Variables: map[string]string{"DATABASE": "shared"}}
	for _, route := range []string{"route-a", "route-b"} {
		for _, name := range []string{"provision", "override", "teardown"} {
			task := config.Task{Name: name, Namespace: "apps", Target: "db.${DEV_NAMESPACE}/${DEV_VAR_DATABASE}/${DEV_ROUTE}", Manifest: "task.json", Timeout: "5m"}
			if err := c.RunTask(context.Background(), task, "apps", "", "rev", "alice", route); err != nil {
				t.Fatal(err)
			}
		}
	}
	leases := 0
	for key := range r.objects {
		if strings.Contains(key, "/lease/") {
			leases++
		}
	}
	if leases != 2 {
		t.Fatalf("expected one lease per route, got %d", leases)
	}
	for _, route := range []string{"route-a", "route-b"} {
		target := "db.apps/shared/" + route
		data := r.objects["apps/lease/"+lockName(target)]
		if len(data) == 0 || !strings.Contains(string(data), target) || strings.Contains(string(data), "${DEV_") {
			t.Fatalf("resolved route target missing: %s", data)
		}
	}
}
