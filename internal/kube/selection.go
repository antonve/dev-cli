package kube

import (
	"net/http"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/deeplink"
)

// Selection is handled by Envoy, not the guest application. Method matches
// outrank header matches at equal path specificity in Gateway API. Without
// them an existing branch's Cookie match would beat a new query-only match.
// GET and HEAD cover navigation without changing selection on mutation calls.
func selectionRule(cfg config.Config, d config.Deployable, selection string, maxAge int, backends []any) any {
	matches := []any{}
	for _, method := range []string{"GET", "HEAD"} {
		matches = append(matches, map[string]any{
			"path":        map[string]any{"type": "PathPrefix", "value": d.PublicPath},
			"method":      method,
			"queryParams": []any{map[string]any{"name": deeplink.Parameter, "type": "Exact", "value": selection}},
		})
	}
	cookie := http.Cookie{Name: cfg.CookieName, Value: selection, Path: "/", Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
	if selection == deeplink.Base {
		cookie.Value, cookie.MaxAge = "", -1
	}
	filters := routeHeaderFilters(selection, true, d.Proxy(cfg).Name != "")
	// Add a separate Set-Cookie header: never replace application cookies.
	filters[1].(map[string]any)["responseHeaderModifier"].(map[string]any)["add"] = []any{map[string]any{"name": "Set-Cookie", "value": cookie.String()}}
	return map[string]any{
		"matches": matches, "backendRefs": backends,
		"filters": filters,
	}
}
