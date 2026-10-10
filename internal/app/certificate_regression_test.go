package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/naming"
)

type certificateRunner struct {
	*lifecycleRunner
	created, ready bool
	deadline       time.Time
}

func (r *certificateRunner) Run(ctx context.Context, command string, args []string, in io.Reader) ([]byte, error) {
	if command == "kubectl" && len(args) > 5 {
		if args[4] == "get" && args[5] == "httproutes" {
			return []byte(`{"items":[{"metadata":{"generation":1},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True","observedGeneration":1},{"type":"ResolvedRefs","status":"True","observedGeneration":1}]}]}}]}`), nil
		}
		if args[4] == "wait" && strings.HasPrefix(args[5], "certificate/") {
			if !slices.Contains(args, "--for=create") && !r.created {
				return nil, errors.New("certificates.cert-manager.io not found")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 120*time.Second {
				return nil, errors.New("certificate wait has no bounded deadline")
			}
			if slices.Contains(args, "--for=create") {
				r.created, r.deadline = true, deadline
				return nil, nil
			}
			if !deadline.Equal(r.deadline) {
				return nil, errors.New("readiness wait reset the certificate timeout")
			}
			r.ready = true
			return nil, nil
		}
		if args[4] == "get" && args[5] == "pods" {
			return nil, errors.New("sync boundary")
		}
	}
	return r.lifecycleRunner.Run(ctx, command, args, in)
}

func TestUpWaitsForCertificateCreationAndReadiness(t *testing.T) {
	for _, issuer := range []string{"lab-ca-acme"} {
		t.Run(issuer, func(t *testing.T) {
			base, cfg, _ := lifecycleFixture(t)
			cfg.Hooks.BeforeUp = nil
			cfg.Hooks.Deployables = nil
			cfg.ClusterIssuer, cfg.IngressHost = issuer, "app.dev.lab"
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
			defer server.Close()
			previous := http.DefaultClient
			http.DefaultClient = server.Client()
			t.Cleanup(func() { http.DefaultClient = previous })
			cfg.Registry = strings.TrimPrefix(server.URL, "https://")
			b, _ := json.Marshal(cfg)
			if err := os.WriteFile("config.json", b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("metadata.json", []byte(`{"name":"api","kind":"backend","containerPath":"/app/api","imageName":"api","pushTarget":"//:push","buildTarget":"//:api","publicPath":"/","port":8000,"readinessPath":"/readyz"}`), 0600); err != nil {
				t.Fatal(err)
			}
			overlay := naming.Resource("api", naming.RouteKey("alice", "feature/hooks")) + ".ns.svc.cluster.local"
			previousGateway := gateway
			gateway = testWaiter(gatewayServer(t, func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Dev-Backend", overlay) }))
			t.Cleanup(func() { gateway = previousGateway })
			r := &certificateRunner{lifecycleRunner: base}
			var output bytes.Buffer
			err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &output, &output, r)
			if err != nil {
				t.Fatalf("startup failed: %v %s", err, &output)
			}
			if r.created != (issuer != "") || r.ready != (issuer != "") || strings.Contains(output.String(), "warning: branch certificate") {
				t.Fatalf("certificate startup: created=%t ready=%t output=%s", r.created, r.ready, &output)
			}
			for _, raw := range base.objects {
				var object struct {
					Metadata struct{ Annotations map[string]string }
				}
				if err := json.Unmarshal(raw, &object); err != nil {
					t.Fatal(err)
				}
				if got := object.Metadata.Annotations["dev-cli.io/cli-version"]; got != "" && got != version {
					t.Fatalf("provenance version %q differs from CLI %q", got, version)
				}
			}
		})
	}
}
