package kube

// Envoy resolves the upstream formatter after health-based backend selection.
// A route's branch is intent, never evidence that its overlay handled a request.
func routeHeaderFilters(selection string, public, proxy bool) []any {
	remove := []string{"x-dev-selected", "x-dev-backend", "x-dev-proxy-backend"}
	request := map[string]any{"remove": remove}
	request["set"] = []any{map[string]any{"name": "x-dev-branch", "value": selection}}
	set := []any{}
	if public {
		set = append(set, map[string]any{"name": "Cache-Control", "value": "no-store"})
	}
	backend := "x-dev-backend"
	if proxy {
		// Preserve the internal data route's backend; this hop only proves
		// which authentication proxy was contacted, not which API served it.
		backend = "x-dev-proxy-backend"
	}
	set = append(set, map[string]any{"name": "x-dev-selected", "value": selection}, map[string]any{"name": backend, "value": "%UPSTREAM_HOST_NAME%"})
	response := map[string]any{"set": set}
	if !proxy {
		response["remove"] = []string{"x-dev-proxy-backend"}
	}
	return []any{
		map[string]any{"type": "RequestHeaderModifier", "requestHeaderModifier": request},
		map[string]any{"type": "ResponseHeaderModifier", "responseHeaderModifier": response},
	}
}
