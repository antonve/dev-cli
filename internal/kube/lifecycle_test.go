package kube

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

type heartbeatRunner struct {
	captureRunner
	calls []string
}

func (r *heartbeatRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil, nil
}
func TestHeartbeatRenewsLifecycleMarker(t *testing.T) {
	r := &heartbeatRunner{}
	c := Client{Run: r, Context: "dev", Namespace: "ns"}
	if err := c.Heartbeat(context.Background(), "route", time.Hour); err != nil {
		t.Fatal(err)
	}
	call := strings.Join(r.calls, "\n")
	if !strings.Contains(call, "configmap") || strings.Contains(call, "lifecycle-marker!=true") || !strings.Contains(call, "expires-at=") {
		t.Fatalf("marker not renewed: %s", call)
	}
}
