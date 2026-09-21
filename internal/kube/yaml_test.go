package kube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
)

func TestYAMLDependencyAndTask(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("dependency.yaml", `apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: db-${DEV_ROUTE}
      namespace: ${DEV_NAMESPACE}
    data:
      endpoint: postgres
`); err != nil {
		t.Fatal(err)
	}
	r := &resourceRunner{objects: map[string][]byte{}}
	c := Client{Run: r, Context: "dev", Namespace: "apps"}
	d := config.Dependency{Name: "db", Namespace: "apps", Manifest: "dependency.yaml", Retention: "retain"}
	for range 2 {
		if err := c.Provision(context.Background(), d, "alice", "route-a"); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(r.objects["apps/configmap/db-route-a"]), dependencyManaged) {
		t.Fatal("YAML dependency missing ownership")
	}
	if err := c.Provision(context.Background(), d, "bob", "route-a"); err == nil {
		t.Fatal("YAML dependency bypassed ownership")
	}
	if err := osWrite("job.yaml", `apiVersion: batch/v1
kind: Job
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: placeholder
          env:
            - name: PASSWORD
              valueFrom:
                secretKeyRef: {name: db-${DEV_ROUTE}, key: password}
`); err != nil {
		t.Fatal(err)
	}
	task := config.Task{Name: "migrate", Namespace: "apps", Manifest: "job.yaml", Target: "db.apps", Timeout: "1s", Container: "migrate"}
	if err := c.RunTask(context.Background(), task, "locks", "registry/migrate@sha256:abc", "rev", "alice", "route-a"); err != nil {
		t.Fatal(err)
	}
	found := false
	for key, value := range r.objects {
		if strings.HasPrefix(key, "apps/job/") {
			found = true
			for _, expected := range []string{taskManaged, "registry/migrate@sha256:abc", "secretKeyRef", "db-route-a"} {
				if !strings.Contains(string(value), expected) {
					t.Fatalf("Job lost %s", expected)
				}
			}
		}
	}
	if !found {
		t.Fatal("YAML Job not created")
	}
}

func TestYAMLWorkloadTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := osWrite("pod.yaml", `spec:
  serviceAccountName: api
  containers:
    - name: server
      image: placeholder
      env:
        - name: PASSWORD
          valueFrom:
            secretKeyRef: {name: db-${DEV_ROUTE}, key: password}
      resources:
        limits: {memory: 1Gi}
`); err != nil {
		t.Fatal(err)
	}
	r := &captureRunner{}
	d := config.Deployable{Name: "api", Kind: "backend", WorkloadTemplate: "pod.yaml", DevContainer: "server", Port: 8000, ContainerPath: "/app/api"}
	if err := (Client{Run: r, Context: "dev", Namespace: "apps"}).ApplyOverlay(context.Background(), config.Config{Namespace: "apps"}, d, "registry/api@sha256:abc", "alice", "feature", "rev", "main", "base", "route-a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(r.payload) {
		t.Fatal("Kubernetes payload must remain JSON")
	}
	for _, expected := range []string{"registry/api@sha256:abc", "secretKeyRef", "db-route-a", "1Gi", "serviceAccountName"} {
		if !strings.Contains(string(r.payload), expected) {
			t.Fatalf("workload lost %s", expected)
		}
	}
}

func TestInvalidYAMLCannotMutateKubernetes(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, body := range []string{
		"spec: [", "spec: {}\nspec: {}", "null", "- item",
		"spec: {}\n---\nkind: Secret", "spec: {}\n---\n",
	} {
		if err := osWrite("invalid.yaml", body); err != nil {
			t.Fatal(err)
		}
		r := &resourceRunner{objects: map[string][]byte{}}
		c := Client{Run: r, Context: "dev", Namespace: "apps"}
		if err := c.Provision(context.Background(), config.Dependency{Manifest: "invalid.yaml", Namespace: "apps"}, "alice", "route-a"); err == nil {
			t.Fatalf("Provision accepted %q", body)
		}
		if err := c.RunTask(context.Background(), config.Task{Manifest: "invalid.yaml", Namespace: "apps"}, "locks", "", "rev", "alice", "route-a"); err == nil {
			t.Fatalf("RunTask accepted %q", body)
		}
		if err := c.ApplyOverlay(context.Background(), config.Config{Namespace: "apps"}, config.Deployable{Name: "api", WorkloadTemplate: "invalid.yaml"}, "image", "alice", "branch", "rev", "main", "base", "route-a", time.Now()); err == nil {
			t.Fatalf("ApplyOverlay accepted %q", body)
		}
		for _, call := range r.calls {
			if !strings.Contains(call, " get ") {
				t.Fatalf("invalid YAML caused a mutation: %s", call)
			}
		}
	}
}
