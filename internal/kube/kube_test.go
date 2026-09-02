package kube

import (
	"context"
	"encoding/json"
	"io"
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
func (r *captureRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func TestOverlayOwnershipAndIsolation(t *testing.T) {
	r := &captureRunner{}
	c := Client{Run: r, Context: "dev", Namespace: "ns"}
	d := config.Deployable{Name: "hello-api", Kind: "backend", Image: "registry/image@sha256:abc", Port: 8081, ReadinessPath: "/readyz", BinaryPath: "/app/hello-api"}
	if err := c.ApplyOverlay(context.Background(), config.Config{SyncImage: "busybox@sha256:def", Namespace: "ns"}, d, "alice@example.com", "feature/x", "head", "origin/main", "base", "alice-feature-12345678", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := json.Unmarshal(r.payload, &list); err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]any)
	metadata := items[0].(map[string]any)["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels[ManagedLabel] != "dev-cli" || labels[RouteLabel] != "alice-feature-12345678" {
		t.Fatalf("labels %#v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations["dev-cli.io/branch-original"] != "feature/x" {
		t.Fatalf("annotations %#v", annotations)
	}
}
