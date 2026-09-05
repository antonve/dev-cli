package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

type captureRunner struct{ payload []byte }

func (r *captureRunner) Run(_ context.Context, name string, args []string, in io.Reader) ([]byte, error) {
	if in != nil {
		r.payload, _ = io.ReadAll(in)
	}
	return nil, nil
}

func TestSyncDeletionIsLimitedToPreviousManifest(t *testing.T) {
	removed, err := removedSyncPaths("src/old.ts\nsrc/keep.ts\n", []string{"src/keep.ts", "src/new.ts"})
	if err != nil || !reflect.DeepEqual(removed, []string{"src/old.ts"}) {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	for _, bad := range []string{"../outside", "/etc/passwd", "src/../../outside", ".", "src\\other"} {
		if _, err := removedSyncPaths(bad+"\n", nil); err == nil {
			t.Fatalf("accepted unsafe prior path %q", bad)
		}
		if _, err := removedSyncPaths("", []string{bad}); err == nil {
			t.Fatalf("accepted unsafe current path %q", bad)
		}
	}
}

type uploadRunner struct {
	captureRunner
	fail               bool
	upload, activation string
}

func (r *uploadRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	joined := strings.Join(args, " ")
	if len(args) > 4 && args[4] == "cp" {
		r.upload = args[6]
		if r.fail {
			return nil, errors.New("interrupted copy")
		}
	}
	if strings.Contains(joined, "&& mv ") {
		r.activation = joined
	}
	return []byte("1"), nil
}

func TestBinaryUploadIsPublishedOnlyAfterSuccessfulCopy(t *testing.T) {
	for _, fail := range []bool{true, false} {
		r := &uploadRunner{fail: fail}
		c := Client{Run: r, Context: "dev", Namespace: "ns"}
		err := c.SyncBinary(context.Background(), "pod", "/local/binary", "api", "http://localhost:8080/readyz")
		if (err != nil) != fail {
			t.Fatalf("fail=%v: %v", fail, err)
		}
		if !strings.HasPrefix(r.upload, "pod:/work/upload-") {
			t.Fatalf("unsafe upload: %q", r.upload)
		}
		if fail && r.activation != "" {
			t.Fatal("failed upload was activated")
		}
		if !fail && !strings.Contains(r.activation, " /work/next && kill -TERM ") {
			t.Fatal("completed upload not atomically published before restart")
		}
	}
}
func (r *captureRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func TestOverlayOwnershipAndIsolation(t *testing.T) {
	r := &captureRunner{}
	c := Client{Run: r, Context: "dev", Namespace: "ns"}
	d := config.Deployable{Name: "hello-api", Kind: "backend", Port: 8081, ReadinessPath: "/readyz", ContainerPath: "/app/hello-api"}
	if err := c.ApplyOverlay(context.Background(), config.Config{Namespace: "ns"}, d, "registry/image@sha256:abc", "alice@example.com", "feature/x", "head", "origin/main", "base", "alice-feature-12345678", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := json.Unmarshal(r.payload, &list); err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["kind"] != "ConfigMap" {
		t.Fatalf("backend resources %#v", items)
	}
	metadata := items[1].(map[string]any)["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels[ManagedLabel] != "dev-cli" || labels[RouteLabel] != "alice-feature-12345678" {
		t.Fatalf("labels %#v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations["dev-cli.io/branch-original"] != "feature/x" {
		t.Fatalf("annotations %#v", annotations)
	}
	containers := items[1].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 || containers[0].(map[string]any)["image"] != "registry/image@sha256:abc" {
		t.Fatalf("containers %#v", containers)
	}
}

func TestRoutesUseOverlayAndBaseIndependently(t *testing.T) {
	r := &captureRunner{}
	c := Client{Run: r, Context: "dev", Namespace: "ns"}
	cfg := config.Config{IngressHost: "playground.dev.lab", CookieName: "dev_branch", GatewayName: "dev", GatewayNamespace: "gateway-system"}
	ds := []config.Deployable{
		{Name: "hello-api", PublicPath: "/api", Port: 8081},
		{Name: "echo-api", InternalHost: "echo-api.internal", Port: 8082},
	}
	if err := c.ApplyRoutes(context.Background(), cfg, ds, map[string]bool{"hello-api": true}, "alice", "feature/x", "head", "origin/main", "base", "alice-feature-12345678", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := json.Unmarshal(r.payload, &list); err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]any)
	publicBackend := items[3].(map[string]any)["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["name"]
	internalBackend := items[4].(map[string]any)["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["name"]
	if publicBackend != naming.Resource("active-hello-api", "alice-feature-12345678") || internalBackend != "echo-api" {
		t.Fatalf("backends public=%v internal=%v", publicBackend, internalBackend)
	}
	for i, want := range []bool{false, true} {
		item := items[i].(map[string]any)
		spec := item["spec"].(map[string]any)
		if item["kind"] != "Backend" || spec["fallback"] != want {
			t.Fatalf("wrong failover tier: %#v", item)
		}
		fqdn := spec["endpoints"].([]any)[0].(map[string]any)["fqdn"].(map[string]any)["hostname"].(string)
		if !strings.HasSuffix(fqdn, ".ns.svc.cluster.local") {
			t.Fatalf("backend escaped namespace: %s", fqdn)
		}
	}
}

func TestRouteAdmissionMustMatchCurrentGeneration(t *testing.T) {
	for _, tc := range []struct {
		status     string
		generation int
		want       bool
	}{{"True", 2, true}, {"False", 2, false}, {"True", 1, false}} {
		var routes ObjectList
		data := fmt.Sprintf(`{"items":[{"metadata":{"generation":2},"status":{"parents":[{"conditions":[{"type":"Accepted","status":%q,"observedGeneration":%d},{"type":"ResolvedRefs","status":"True","observedGeneration":2}]}]}}]}`, tc.status, tc.generation)
		if err := json.Unmarshal([]byte(data), &routes); err != nil {
			t.Fatal(err)
		}
		if got := routes.RoutesReady(); got != tc.want {
			t.Fatalf("status=%s generation=%d: got %v", tc.status, tc.generation, got)
		}
	}
	if (ObjectList{}).RoutesReady() {
		t.Fatal("missing routes reported ready")
	}
}

type cleanupRunner struct {
	captureRunner
	objects string
	deleted []string
}

func (r *cleanupRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	if args[4] == "get" {
		if args[5] != ownedResources {
			return nil, fmt.Errorf("cleanup did not scan all owned resources")
		}
		return []byte(r.objects), nil
	}
	r.deleted = append(r.deleted, strings.Join(args, " "))
	return nil, nil
}

func TestCleanupIncludesOrphanRoutesAndKeepsRenewedGroups(t *testing.T) {
	r := &cleanupRunner{objects: `{"items":[
		{"kind":"HTTPRoute","metadata":{"labels":{"dev-cli.io/route":"orphan"},"annotations":{"dev-cli.io/expires-at":"2026-01-01T00:00:00Z"}}},
		{"kind":"Backend","metadata":{"labels":{"dev-cli.io/route":"active"},"annotations":{"dev-cli.io/expires-at":"2026-01-01T00:00:00Z"}}},
		{"kind":"Deployment","metadata":{"labels":{"dev-cli.io/route":"active"},"annotations":{"dev-cli.io/expires-at":"2026-01-03T00:00:00Z"}}},
		{"kind":"Backend","metadata":{"labels":{},"annotations":{"dev-cli.io/expires-at":"2026-01-01T00:00:00Z"}}}
	]}`}
	c := Client{Run: r, Context: "dev", Namespace: "ns"}
	n, err := c.Cleanup(context.Background(), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 || len(r.deleted) != 1 {
		t.Fatalf("removed=%d deletes=%v err=%v", n, r.deleted, err)
	}
	if !strings.Contains(r.deleted[0], ManagedLabel+"=dev-cli,"+RouteLabel+"=orphan") || !strings.Contains(r.deleted[0], ownedResources) {
		t.Fatalf("unsafe or incomplete cleanup: %s", r.deleted[0])
	}
}
