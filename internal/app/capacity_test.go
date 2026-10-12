package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullNode leaves 100Mi of memory for new pods.
const fullNode = `{"items":[{"metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"1","memory":"100Mi"}}}]}`

func writeWorkerTemplate(t *testing.T, configPath, memory string) {
	t.Helper()
	template := "spec:\n  containers:\n    - name: app\n      command: [/release-worker]\n" +
		"      resources: {requests: {cpu: 25m, memory: " + memory + "}}\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "worker.yaml"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertNothingChanged(t *testing.T, r *productionRunner) {
	t.Helper()
	if len(r.pushes) > 0 || len(r.events) > 0 || len(r.objects) > 0 {
		t.Fatalf("capacity refusal changed the cluster: pushes=%v events=%v objects=%d", r.pushes, r.events, len(r.objects))
	}
}

func TestUpRefusesBeforeAnyChangeWhenNoNodeFits(t *testing.T) {
	r, path, _ := productionFixture(t, false)
	writeWorkerTemplate(t, path, "128Mi")
	r.nodes = fullNode
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", path, "--owner", "alice"}, &out, &out, r)
	if err == nil || !strings.Contains(err.Error(), "insufficient cluster capacity") {
		t.Fatalf("up deployed beyond cluster capacity: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "memory=128Mi") || !strings.Contains(out.String(), "node-a") {
		t.Fatalf("refusal does not show what is needed and free:\n%s", out.String())
	}
	assertNothingChanged(t, r)
}

func TestCapacityCommandOnlyReports(t *testing.T) {
	for name, tc := range map[string]struct {
		memory string
		fits   bool
	}{
		"fits":         {"64Mi", true},
		"does not fit": {"128Mi", false},
	} {
		t.Run(name, func(t *testing.T) {
			r, path, _ := productionFixture(t, false)
			writeWorkerTemplate(t, path, tc.memory)
			r.nodes = fullNode
			var out bytes.Buffer
			err := run(context.Background(), []string{"capacity", "--config", path, "--owner", "alice"}, &out, &out, r)
			if (err == nil) != tc.fits {
				t.Fatalf("fits=%t err=%v\n%s", tc.fits, err, out.String())
			}
			if !strings.Contains(out.String(), "worker needs cpu=25m memory="+tc.memory) {
				t.Fatalf("report does not show the worker's needs:\n%s", out.String())
			}
			assertNothingChanged(t, r)
		})
	}
}

func TestDevelopmentCapacityCountsTheDefaultRuntimeRequests(t *testing.T) {
	base, cfg, _ := lifecycleFixture(t)
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile("config.json", b, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := `{"name":"worker","kind":"worker","containerPath":"/app/worker","imageName":"worker","pushTarget":"//:push"}`
	if err := os.WriteFile("metadata.json", []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	base.nodes = `{"items":[{"metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"1","memory":"16Mi"}}}]}`
	var out bytes.Buffer
	err := run(context.Background(), []string{"capacity", "--config", "config.json", "--owner", "alice"}, &out, &out, base)
	if err == nil || !strings.Contains(out.String(), "worker needs cpu=10m memory=32Mi") {
		t.Fatalf("the development runtime's default requests were not counted: %v\n%s", err, out.String())
	}
}
