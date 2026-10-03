package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

func TestSameProfileRerunPreservesRecordedCleanupScope(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	route := naming.RouteKey("alice", "feature/hooks")
	cfg.Variables = map[string]string{"DATABASE": "new-route-db"}
	marker := kube.Lifecycle{Owner: "alice", Profile: "default", Variables: map[string]string{"DATABASE": "old-route-db"}, Hooks: cfg.Hooks, Deployables: []string{"worker"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "old-rev", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.objects["ns/deployment/worker-old"] = []byte(`{"metadata":{"labels":{"dev-cli.io/service":"worker"}},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DATABASE","value":"old-route-db"}]}]}}}}`)
	r.failTask = "before"
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile("config.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("after.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"busybox","env":[{"name":"DATABASE","value":"${DEV_VAR_DATABASE}"}]}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.events = nil
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "run dev down first") {
		t.Errorf("expected variable-scope refusal: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "old-route-db") || strings.Contains(err.Error(), "new-route-db")) {
		t.Error("refusal exposed variable values")
	}
	t.Logf("up returned %v; events=%v", err, r.events)
	recorded, err := k.ReadLifecycle(context.Background(), route)
	if err != nil || recorded == nil {
		t.Fatalf("marker read: %v", err)
	}
	if recorded.Variables["DATABASE"] != "old-route-db" {
		t.Errorf("same-profile up lost old live cleanup scope: marker DATABASE=%q, existing deployment still uses old-route-db", recorded.Variables["DATABASE"])
	}
	if eventIndex(r.events, "push") >= 0 || eventIndex(r.events, "overlay") >= 0 {
		t.Fatal("reproduction unexpectedly changed deployments")
	}
	r.failTask = ""
	if err := teardown(context.Background(), k, cfg, "alice", route, io.Discard); err != nil {
		t.Fatal(err)
	}
	found := false
	for key, data := range r.objects {
		if strings.Contains(key, "/job/") && jobTask(data) == "after" {
			found = true
			t.Log("teardown hook rendered the recorded scope")
			if !strings.Contains(string(data), "old-route-db") {
				t.Errorf("teardown lost old resource scope: rendered hook targets new-route-db")
			}
		}
	}
	if !found {
		t.Fatal("teardown hook missing")
	}
}

func TestSameProfileNilAndEmptyVariablesRemainCompatible(t *testing.T) {
	for _, variables := range []map[string]string{nil, {}} {
		t.Run(fmt.Sprintf("length-%d", len(variables)), func(t *testing.T) {
			r, cfg, k := lifecycleFixture(t)
			cfg.Variables = variables
			route := naming.RouteKey("alice", "feature/hooks")
			marker := kube.Lifecycle{Owner: "alice", Profile: "default", Variables: map[string]string{}, Hooks: cfg.Hooks}
			if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			r.failTask = "before"
			data, _ := json.Marshal(cfg)
			_ = os.WriteFile("config.json", data, 0600)
			_ = os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600)
			var out bytes.Buffer
			err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
			if err == nil || !strings.Contains(err.Error(), "task before failed") {
				t.Fatalf("nil/empty variables wrongly refused: %v", err)
			}
		})
	}
}
