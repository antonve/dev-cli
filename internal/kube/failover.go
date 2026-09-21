package kube

import (
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

// Envoy owns health-based selection, including while the CLI is disconnected.
// Both endpoints use DNS so deleting an overlay Service does not invalidate
// the HTTPRoute's Backend object references.
func (c Client) failoverResources(cfg config.Config, d config.Deployable, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time, created string) ([]any, []any) {
	var items, refs []any
	for _, fallback := range []bool{false, true} {
		service, namespace, port, prefix := naming.Resource(d.Name, route), d.WorkloadNamespace(cfg), d.OverlayPort(), "active-"
		if fallback {
			base := d.Base(cfg)
			service, namespace, port, prefix = base.Name, base.Namespace, base.Port, "fallback-"
		}
		name := naming.Resource(prefix+d.Name, route)
		items = append(items, map[string]any{
			"apiVersion": "gateway.envoyproxy.io/v1alpha1", "kind": "Backend",
			"metadata": map[string]any{"name": name, "labels": labels(owner, route, d.Name), "annotations": annotations(owner, branch, revision, baseRef, baseRevision, "", expiry, created)},
			"spec":     map[string]any{"fallback": fallback, "endpoints": []any{map[string]any{"fqdn": map[string]any{"hostname": service + "." + namespace + ".svc.cluster.local", "port": port}}}},
		})
		refs = append(refs, map[string]any{"group": "gateway.envoyproxy.io", "kind": "Backend", "name": name, "port": port})
	}
	name := naming.Resource("route-"+d.Name, route)
	items = append(items, map[string]any{
		"apiVersion": "gateway.envoyproxy.io/v1alpha1", "kind": "BackendTrafficPolicy",
		"metadata": map[string]any{"name": name, "labels": labels(owner, route, d.Name), "annotations": annotations(owner, branch, revision, baseRef, baseRevision, "", expiry, created)},
		"spec": map[string]any{
			"dns":         map[string]any{"dnsRefreshRate": "1s", "respectDnsTtl": false},
			"targetRefs":  []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": name}},
			"healthCheck": map[string]any{"active": map[string]any{"type": "HTTP", "interval": "1s", "timeout": "1s", "healthyThreshold": 1, "unhealthyThreshold": 1, "http": map[string]any{"path": d.ReadinessPath}}},
		},
	})
	return items, refs
}
