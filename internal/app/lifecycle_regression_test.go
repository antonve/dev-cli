package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/localstate"
	"github.com/antonve/dev-cli/internal/naming"
)

type startupHeartbeatRunner struct {
	*lifecycleRunner
	mu                  sync.Mutex
	cfg                 config.Config
	delayed             bool
	heartbeats, cleaned int
	cleanupError        error
}

func (r *startupHeartbeatRunner) Run(ctx context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	r.mu.Lock()
	if command == "kubectl" && len(args) > 5 && args[4] == "annotate" {
		r.heartbeats++
		expiry := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "dev-cli.io/expires-at=") {
				expiry = strings.TrimPrefix(arg, "dev-cli.io/expires-at=")
			}
		}
		for key, b := range r.objects {
			if !strings.Contains(key, "/configmap/lifecycle-") {
				continue
			}
			var object map[string]any
			_ = json.Unmarshal(b, &object)
			object["metadata"].(map[string]any)["annotations"].(map[string]any)["dev-cli.io/expires-at"] = expiry
			r.objects[key], _ = json.Marshal(object)
		}
		r.mu.Unlock()
		return nil, nil
	}
	out, err := r.lifecycleRunner.Run(ctx, command, args, in)
	delay := command == "kubectl" && len(args) > 5 && args[4] == "get" && args[5] == "job" && jobTask(out) == "before" && !r.delayed
	if delay {
		r.delayed = true
	}
	r.mu.Unlock()
	if delay {
		time.Sleep(1200 * time.Millisecond)
		k := kube.Client{Run: r, Context: "dev", Namespace: "ns"}
		r.cleaned, r.cleanupError = cleanup(context.Background(), k, r.cfg, time.Now(), true, io.Discard)
	}
	return out, err
}
func TestStartupRenewsMarkerDuringSlowHook(t *testing.T) {
	base, cfg, _ := lifecycleFixture(t)
	base.stopOverlay = true
	cfg.TTL = "1s"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	cfg.Registry = strings.TrimPrefix(server.URL, "https://")
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile("config.json", b, 0600)
	_ = os.WriteFile("metadata.json", []byte(`{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`), 0600)
	r := &startupHeartbeatRunner{lifecycleRunner: base, cfg: cfg}
	var output bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &output, &output, r)
	if err == nil || err.Error() != "overlay boundary" {
		t.Fatalf("startup boundary: %v", err)
	}
	if r.heartbeats == 0 || r.cleaned != 0 || r.cleanupError != nil || eventIndex(base.events, "delete-marker") >= 0 {
		t.Fatalf("active startup torn down: heartbeats=%d removed=%d err=%v events=%v", r.heartbeats, r.cleaned, r.cleanupError, base.events)
	}
}
func TestInterruptedTeardownRetainsLearnedWorkerHook(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	marker := kube.Lifecycle{Owner: "alice", Hooks: cfg.Hooks, Deployables: []string{"frontend"}}
	if err := k.WriteLifecycle(context.Background(), marker, "branch", "rev", "main", "base", "route", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.objects["ns/deployment/worker-route"] = []byte(`{"metadata":{"labels":{"dev-cli.io/service":"worker"}}}`)
	r.failWait = true
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err == nil {
		t.Fatal("expected interrupted pod wait")
	}
	recorded, err := k.ReadLifecycle(context.Background(), "route")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range recorded.Deployables {
		if name == "worker" {
			found = true
		}
	}
	if !found || recorded.OverlaysStopped {
		t.Fatalf("lost learned service before deletion: %+v", recorded)
	}
	r.failWait = false
	r.events = nil
	if err := teardown(context.Background(), k, cfg, "alice", "route", io.Discard); err != nil {
		t.Fatal(err)
	}
	if eventIndex(r.events, "job:stop") < 0 || eventIndex(r.events, "delete-marker") < 0 {
		t.Fatalf("retry skipped learned worker: %v", r.events)
	}
}
func TestCleanupStopsRecordedLocalOwnerBeforeHooks(t *testing.T) {
	r, cfg, k := lifecycleFixture(t)
	putMarker(t, k, cfg, "alice", "route")
	key := "ns/configmap/" + naming.Resource("lifecycle", "route")
	var object map[string]any
	_ = json.Unmarshal(r.objects[key], &object)
	data := object["data"].(map[string]any)
	var marker map[string]any
	_ = json.Unmarshal([]byte(data["hooks.json"].(string)), &marker)
	marker["checkout"] = r.root
	encoded, _ := json.Marshal(marker)
	data["hooks.json"] = string(encoded)
	r.objects[key], _ = json.Marshal(object)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := localstate.New(r.root, "route")
	release, err := owner.Start(cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan struct{})
	go func() { <-ctx.Done(); release(); close(done) }()
	n, err := cleanup(context.Background(), k, cfg, time.Now(), true, io.Discard)
	if err != nil || n != 1 || owner.Running() {
		t.Fatalf("cleanup did not stop owner: removed=%d err=%v running=%t", n, err, owner.Running())
	}
	select {
	case <-done:
	default:
		t.Fatal("cleanup completed before owner work stopped")
	}
	if eventIndex(r.events, "job:after") < 0 {
		t.Fatal("teardown hook did not run")
	}
}
