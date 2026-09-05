package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
)

type captureRunner struct{ payload []byte }

func (r *captureRunner) Run(_ context.Context, name string, args []string, in io.Reader) ([]byte, error) {
	if in != nil {
		r.payload, _ = io.ReadAll(in)
	}
	return nil, nil
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
	publicBackend := items[0].(map[string]any)["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["name"]
	internalBackend := items[1].(map[string]any)["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["name"]
	if publicBackend != "hello-api-dev-alice-feature-12345678" || internalBackend != "echo-api" {
		t.Fatalf("backends public=%v internal=%v", publicBackend, internalBackend)
	}
}
