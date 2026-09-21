package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/deeplink"
	"github.com/antonve/dev-cli/internal/naming"
)

func TestQuerySelectionRules(t *testing.T) {
	for _, affected := range []bool{false, true} {
		r := &captureRunner{}
		c := Client{Run: r, Context: "dev", Namespace: "ns"}
		cfg := config.Config{IngressHost: "app.dev.lab", CookieName: "dev_branch", TTL: "2h"}
		d := config.Deployable{Name: "web", PublicPath: "/settings", Port: 8080, ReadinessPath: "/readyz"}
		if err := c.ApplyRoutes(context.Background(), cfg, []config.Deployable{d}, map[string]bool{"web": affected}, "alice", "feature", "head", "origin/main", "base", "alice-feature-abc", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		var list struct {
			Items []struct {
				Kind     string
				Metadata struct{ Labels map[string]string }
				Spec     struct {
					Rules []struct {
						Matches []struct {
							Path        struct{ Type, Value string }
							Method      string
							QueryParams []struct{ Name, Type, Value string }
						}
						Filters []struct {
							Type                  string
							RequestHeaderModifier struct {
								Set    []struct{ Name, Value string }
								Remove []string
							}
							ResponseHeaderModifier struct {
								Add, Set []struct{ Name, Value string }
							}
						}
						BackendRefs []struct{ Name string }
					}
				}
			}
		}
		if err := json.Unmarshal(r.payload, &list); err != nil {
			t.Fatal(err)
		}
		for _, item := range list.Items {
			if item.Kind != "HTTPRoute" {
				continue
			}
			if item.Metadata.Labels[RouteLabel] != "alice-feature-abc" || item.Metadata.Labels[OwnerLabel] != "alice" {
				t.Fatal("selection escaped owner cleanup")
			}
			if len(item.Spec.Rules) != 3 {
				t.Fatalf("rules: %d", len(item.Spec.Rules))
			}
			for i, selection := range []string{"alice-feature-abc", deeplink.Base} {
				rule := item.Spec.Rules[i+1]
				if len(rule.Matches) != 2 {
					t.Fatal("must match GET and HEAD")
				}
				for j, method := range []string{"GET", "HEAD"} {
					m := rule.Matches[j]
					if m.Method != method || m.Path.Type != "PathPrefix" || m.Path.Value != d.PublicPath || len(m.QueryParams) != 1 || m.QueryParams[0].Name != deeplink.Parameter || m.QueryParams[0].Type != "Exact" || m.QueryParams[0].Value != selection {
						t.Fatalf("wrong precedence or selection match: %+v", m)
					}
				}
				wantBackend := d.Name
				if i == 0 && affected {
					wantBackend = naming.Resource("active-web", selection)
				}
				if rule.BackendRefs[0].Name != wantBackend {
					t.Fatalf("wrong backend: %+v", rule.BackendRefs)
				}
				request := rule.Filters[0].RequestHeaderModifier
				if i == 0 && (len(request.Set) != 1 || request.Set[0].Name != "x-dev-branch" || request.Set[0].Value != selection) {
					t.Fatal("query did not normalize header")
				}
				if i == 1 && (!slices.Contains(request.Remove, "x-dev-branch") || len(request.Set) != 0) {
					t.Fatal("base must remove branch context")
				}
				response := rule.Filters[1].ResponseHeaderModifier
				if len(response.Add) != 1 || response.Add[0].Name != "Set-Cookie" {
					t.Fatal("must append cookie without replacing application cookies")
				}
				cookies := (&http.Response{Header: http.Header{"Set-Cookie": []string{response.Add[0].Value}}}).Cookies()
				if len(cookies) != 1 {
					t.Fatal("invalid cookie")
				}
				cookie := cookies[0]
				if cookie.Name != cfg.CookieName || cookie.Path != "/" || cookie.Domain != "" || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
					t.Fatalf("unsafe scope: %+v", cookie)
				}
				if i == 0 && (cookie.Value != selection || cookie.MaxAge != 7200) {
					t.Fatal("wrong selected cookie")
				}
				if i == 1 && (cookie.Value != "" || cookie.MaxAge != -1) {
					t.Fatal("base cookie not expired")
				}
				if len(response.Set) != 4 || response.Set[0].Name != "Cache-Control" || response.Set[0].Value != "no-store" {
					t.Fatal("selection response cacheable")
				}
			}
		}
	}
}
