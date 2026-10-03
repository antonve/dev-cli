package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

func TestTeardownUsesRecordedVariablesFromAnotherConfig(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	cfg.Variables = map[string]string{"DATABASE": "wrong-current-checkout"}
	if err := os.WriteFile("after.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"busybox","env":[{"name":"DATABASE","value":"${DEV_VAR_DATABASE}"}]}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	marker := kube.Lifecycle{Owner: "alice", Profile: "isolated", Variables: map[string]string{"DATABASE": "recorded-route-db"}, Hooks: cfg.Hooks, Deployables: []string{"worker"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", "route", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err != nil {
		t.Fatal(err)
	}
	found := false
	for key, data := range r.objects {
		if strings.Contains(key, "/job/") && jobTask(data) == "after" {
			found = true
			if !strings.Contains(string(data), "recorded-route-db") || strings.Contains(string(data), "wrong-current-checkout") {
				t.Fatalf("caller values used: %s", data)
			}
		}
	}
	if !found {
		t.Fatal("teardown hook not rendered")
	}
}
func TestLiveRouteProfileSwitchRefusedBeforeHookPushOrOverlay(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	cfg.Profiles = []config.Profile{{Name: "isolated", WhenChanged: []string{"change.txt"}}}
	route := naming.RouteKey("alice", "feature/hooks")
	marker := kube.Lifecycle{Owner: "alice", Profile: "default", Hooks: cfg.Hooks, Deployables: []string{"worker"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile("config.json", data, 0600)
	_ = os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600)
	r.events = nil
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "runs profile default; run dev down first") || len(r.events) > 0 {
		t.Fatalf("unsafe switch: %v %v", err, r.events)
	}
}
func TestStatusShowsRecordedProfile(t *testing.T) {
	_, cfg, k := lifecycleFixture(t)
	marker := kube.Lifecycle{Owner: "alice", Profile: "isolated", Variables: map[string]string{"DATABASE": "db"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", "route", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := status(context.Background(), k, cfg, "alice", "branch", "route", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"profile": "isolated"`) {
		t.Fatalf("profile missing: %s", out.String())
	}
}
func TestTaskUsesRecordedProfileAndVariables(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	cfg.Variables = map[string]string{"DATABASE": "default-db"}
	cfg.Profiles = []config.Profile{{Name: "isolated", WhenChanged: []string{"change.txt"}, Variables: map[string]string{"DATABASE": "new-config-db"}}}
	route := naming.RouteKey("alice", "feature/hooks")
	marker := kube.Lifecycle{Owner: "alice", Profile: "isolated", Variables: map[string]string{"DATABASE": "recorded-db"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile("before.json", []byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hook"},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"hook","image":"busybox","env":[{"name":"DATABASE","value":"${DEV_VAR_DATABASE}"}]}]}}}}`), 0600)
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile("config.json", data, 0600)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"task", "--config", "config.json", "--owner", "alice", "before"}, &out, &out, r); err != nil {
		t.Fatal(err)
	}
	found := false
	for key, data := range r.objects {
		if strings.Contains(key, "/job/") {
			found = true
			if !strings.Contains(string(data), "recorded-db") {
				t.Fatalf("recorded variables not used: %s", data)
			}
		}
	}
	if !found {
		t.Fatal("no task Job")
	}
}
