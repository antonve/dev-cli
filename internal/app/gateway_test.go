package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	testOverlay = "api-dev-route.ns.svc.cluster.local"
	testBase    = "api.ns.svc.cluster.local"
)

// gatewayServer answers every host on one TLS listener, as the shared public
// gateway does, and returns a client that reaches it for any branch hostname.
func gatewayServer(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	client := *gateway.client
	client.Transport = transport
	return &client
}

// backends answers with the listed X-Dev-Backend values in a repeating cycle.
func backends(values ...string) (http.HandlerFunc, *atomic.Int32) {
	calls := &atomic.Int32{}
	return func(w http.ResponseWriter, _ *http.Request) {
		i := int(calls.Add(1)) - 1
		if value := values[i%len(values)]; value != "" {
			w.Header().Set("X-Dev-Backend", value)
		}
	}, calls
}

func testWaiter(client *http.Client) gatewayWaiter {
	return gatewayWaiter{client: client, interval: time.Millisecond, timeout: 300 * time.Millisecond}
}

func TestGatewayWait(t *testing.T) {
	probe := gatewayProbe{url: "https://route.app.dev.lab/readyz", backend: testOverlay}
	for name, tc := range map[string]struct {
		backends  []string
		wantCalls int32
		wantErr   []string
	}{
		"converges":               {[]string{testOverlay}, gatewayConsecutive, nil},
		"flaps then converges":    {[]string{testOverlay, testOverlay, testBase, testOverlay}, 6, nil},
		"never leaves base":       {[]string{testBase}, 0, []string{"route.app.dev.lab", testBase, testOverlay}},
		"missing backend header":  {[]string{""}, 0, []string{"route.app.dev.lab", "no X-Dev-Backend"}},
		"overlay never converges": {[]string{testOverlay, testBase}, 0, []string{"route.app.dev.lab", testOverlay}},
	} {
		t.Run(name, func(t *testing.T) {
			handler, calls := backends(tc.backends...)
			err := testWaiter(gatewayServer(t, handler)).wait(context.Background(), []gatewayProbe{probe}, func(string) {})
			if tc.wantErr == nil {
				if err != nil || calls.Load() != tc.wantCalls {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
				return
			}
			if err == nil {
				t.Fatal("wait succeeded without a converged branch backend")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error lacks %q: %v", want, err)
				}
			}
		})
	}
}

func TestGatewayWaitRejectsServerErrorsFromTheBranch(t *testing.T) {
	client := gatewayServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Dev-Backend", testOverlay)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	probe := gatewayProbe{url: "https://route.app.dev.lab/readyz", backend: testOverlay}
	err := testWaiter(client).wait(context.Background(), []gatewayProbe{probe}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a 503 counted as branch service: %v", err)
	}
}

func TestGatewayWaitDoesNotFollowRedirects(t *testing.T) {
	client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			t.Errorf("followed redirect to %s", r.URL)
		}
		w.Header().Set("X-Dev-Backend", testOverlay)
		http.Redirect(w, r, "https://elsewhere.dev.lab/", http.StatusFound)
	})
	probe := gatewayProbe{url: "https://route.app.dev.lab/login", backend: testOverlay}
	if err := testWaiter(client).wait(context.Background(), []gatewayProbe{probe}, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayProbesCoverOnlyDirectPublicRoutes(t *testing.T) {
	cfg := config.Config{Namespace: "routing", IngressHost: "app.dev.lab", PublicHosts: []string{"app.dev.lab", "account.dev.lab"}}
	ds := []config.Deployable{
		{Name: "web", Kind: "frontend", Namespace: "apps", PublicPath: "/", ReadinessPath: "/healthz"},
		{Name: "auth", Kind: "frontend", PublicPath: "/", PublicHost: "account.dev.lab", ReadinessPath: "/login"},
		{Name: "docs", Kind: "frontend", PublicPath: "/docs", ReadinessPath: "/healthz"},
		{Name: "worker", Kind: "worker"},
		{Name: "internal", Kind: "backend", InternalHost: "internal.svc"},
		{Name: "api", Kind: "backend", PublicPath: "/", InternalHost: "api.svc", PublicProxy: config.ObjectRef{Name: "proxy", Port: 4455}},
	}
	got := gatewayProbes(cfg, ds, "route")
	want := []gatewayProbe{
		{url: "https://route.app.dev.lab/healthz", backend: kube.OverlayHost(cfg, ds[0], "route")},
		{url: "https://route.account.dev.lab/login", backend: kube.OverlayHost(cfg, ds[1], "route")},
		{url: "https://route.app.dev.lab/docs", backend: kube.OverlayHost(cfg, ds[2], "route")},
	}
	if len(got) != len(want) {
		t.Fatalf("probes:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	if want[0].backend != naming.Resource("web", "route")+".apps.svc.cluster.local" {
		t.Fatalf("overlay host ignores the workload namespace: %s", want[0].backend)
	}
}

// gatewayUp runs a no-watch startup for one public backend behind a gateway
// that answers with handler.
func gatewayUp(t *testing.T, handler http.HandlerFunc) (string, error) {
	t.Helper()
	base, cfg, _ := lifecycleFixture(t)
	cfg.Hooks = config.Hooks{}
	cfg.IngressHost = "app.dev.lab"
	serveRegistry(t, &cfg)
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile("config.json", b, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := `{"name":"api","kind":"backend","containerPath":"/app/api","imageName":"api","pushTarget":"//:push",` +
		`"buildTarget":"//:api","publicPath":"/","port":8000,"readinessPath":"/readyz"}`
	if err := os.WriteFile("metadata.json", []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	previous := gateway
	gateway = testWaiter(gatewayServer(t, handler))
	t.Cleanup(func() { gateway = previous })
	var out bytes.Buffer
	err := run(context.Background(), []string{"up", "--config", "config.json", "--owner", "alice", "--no-watch"}, &out, &out, &certificateRunner{lifecycleRunner: base})
	return out.String(), err
}

func TestUpOpensTheEnvironmentOnlyAfterTheGatewaySelectsTheBranch(t *testing.T) {
	route := naming.RouteKey("alice", "feature/hooks")
	overlay := naming.Resource("api", route) + ".ns.svc.cluster.local"
	var hosts atomic.Value
	out, err := gatewayUp(t, func(w http.ResponseWriter, r *http.Request) {
		hosts.Store(r.Host + r.URL.Path)
		w.Header().Set("X-Dev-Backend", overlay)
	})
	if err != nil {
		t.Fatalf("startup failed: %v %s", err, out)
	}
	if got := hosts.Load(); got != route+".app.dev.lab/readyz" {
		t.Fatalf("probed %v", got)
	}
	if !strings.Contains(out, "Open environment: https://"+route+".app.dev.lab/") {
		t.Fatalf("environment link missing: %s", out)
	}
}

func TestUpFailsWhenTheGatewayKeepsServingBase(t *testing.T) {
	route := naming.RouteKey("alice", "feature/hooks")
	out, err := gatewayUp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Dev-Backend", "api.ns.svc.cluster.local")
	})
	if err == nil || !strings.Contains(err.Error(), route+".app.dev.lab") || !strings.Contains(err.Error(), "api.ns.svc.cluster.local") {
		t.Fatalf("expected gateway convergence error naming host and backend: %v %s", err, out)
	}
	if strings.Contains(out, "Open environment") {
		t.Fatalf("printed an environment link served by base: %s", out)
	}
}
