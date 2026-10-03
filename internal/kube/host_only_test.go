package kube

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

type hostOnlyRunner struct {
	captureRunner
	deletes []string
}

func (r *hostOnlyRunner) Run(ctx context.Context, name string, args []string, in io.Reader) ([]byte, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "delete httproute") {
		r.deletes = append(r.deletes, joined)
	}
	return r.captureRunner.Run(ctx, name, args, in)
}
func TestHostOnlyRoutingAndLegacyCleanup(t *testing.T) {
	r := &hostOnlyRunner{}
	cfg := config.Config{Namespace: "ns", IngressHost: "app.dev.lab", PublicHosts: []string{"app.dev.lab"}, ClusterIssuer: "lab-ca-acme"}
	ds := []config.Deployable{{Name: "web", PublicPath: "/", Port: 3000}, {Name: "api", PublicPath: "/api", InternalHost: "api.internal", Port: 8000, PublicProxy: config.ObjectRef{Name: "auth", Port: 4455}}}
	const route = "alice-feature-abcd1234"
	if err := (Client{Run: r, Context: "dev", Namespace: "ns"}).ApplyRoutes(context.Background(), cfg, ds, map[string]bool{"web": true, "api": true}, "alice", "feature", "head", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []map[string]any }
	if err := json.Unmarshal(r.payload, &list); err != nil {
		t.Fatal(err)
	}
	for _, object := range list.Items {
		if object["kind"] != "HTTPRoute" {
			continue
		}
		encoded, _ := json.Marshal(object)
		for _, forbidden := range []string{`"Cookie"`, `"queryParams"`, `"Set-Cookie"`} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("obsolete branch selection %s: %s", forbidden, encoded)
			}
		}
	}
	for _, name := range []string{naming.Resource("route-web", route), naming.Resource("route-public-api", route)} {
		found := false
		for _, call := range r.deletes {
			if strings.Contains(call, "metadata.name="+name) && strings.Contains(call, OwnerLabel+"=alice") && strings.Contains(call, RouteLabel+"="+route) && strings.Contains(call, ManagedLabel+"=dev-cli") {
				found = true
			}
		}
		if !found {
			t.Fatalf("legacy route not retired within ownership boundary: %s, calls=%v", name, r.deletes)
		}
	}
}
