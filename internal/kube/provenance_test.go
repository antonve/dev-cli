package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
)

func TestRouteResponseProvenance(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, affected := range []bool{false, true} {
			t.Run(fmt.Sprintf("proxy=%t/affected=%t", proxy, affected), func(t *testing.T) {
				r := &captureRunner{}
				c := Client{Run: r, Context: "dev", Namespace: "ns"}
				cfg := config.Config{Namespace: "ns", IngressHost: "app.dev.lab", CookieName: "dev_branch"}
				d := config.Deployable{Name: "api", PublicPath: "/api", InternalHost: "api.internal", Port: 8080, ReadinessPath: "/readyz"}
				if proxy {
					d.PublicProxy = config.ObjectRef{Name: "auth", Port: 4455}
				}
				const route = "alice-feature-abc"
				if err := c.ApplyRoutes(context.Background(), cfg, []config.Deployable{d}, map[string]bool{"api": affected}, "alice", "feature", "head", "main", "base", route, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				var list struct {
					Items []struct {
						Kind string
						Spec struct {
							Hostnames []string
							Rules     []struct {
								Filters []struct {
									Type                   string
									RequestHeaderModifier  struct{ Remove []string }
									ResponseHeaderModifier struct {
										Set    []struct{ Name, Value string }
										Remove []string
									}
								}
							}
						}
					}
				}
				if err := json.Unmarshal(r.payload, &list); err != nil {
					t.Fatal(err)
				}
				checked := 0
				for _, item := range list.Items {
					if item.Kind != "HTTPRoute" {
						continue
					}
					public := item.Spec.Hostnames[0] == cfg.IngressHost
					for i, rule := range item.Spec.Rules {
						checked++
						if len(rule.Filters) != 2 || rule.Filters[0].Type != "RequestHeaderModifier" || rule.Filters[1].Type != "ResponseHeaderModifier" {
							t.Fatal("every data/cookie/select/clear rule needs provenance filters")
						}
						for _, header := range []string{"x-dev-selected", "x-dev-backend", "x-dev-proxy-backend"} {
							if !slices.Contains(rule.Filters[0].RequestHeaderModifier.Remove, header) {
								t.Fatalf("client can inject %s", header)
							}
						}
						response := rule.Filters[1].ResponseHeaderModifier
						values := map[string]string{}
						for _, header := range response.Set {
							values[header.Name] = header.Value
						}
						selection, backend := route, "x-dev-backend"
						if public && i == 2 {
							selection = "base"
						}
						if public && proxy {
							backend = "x-dev-proxy-backend"
							if _, overwritten := values["x-dev-backend"]; overwritten || slices.Contains(response.Remove, "x-dev-backend") {
								t.Fatal("auth hop destroys the internal API backend evidence")
							}
						} else if !slices.Contains(response.Remove, "x-dev-proxy-backend") {
							t.Fatal("direct backend can inject an unrelated proxy identity")
						}
						if values["x-dev-selected"] != selection || values[backend] != "%UPSTREAM_HOST_NAME%" {
							t.Fatalf("provenance must describe normalized selection and dynamically chosen upstream: %v", values)
						}
					}
				}
				if checked != 4 {
					t.Fatalf("checked %d rules, want internal + public cookie/select/clear", checked)
				}
			})
		}
	}
}
