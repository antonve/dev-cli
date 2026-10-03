package kube

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

func TestBranchHostRoutes(t *testing.T) {
	for _, issuer := range []string{"", "lab-ca-acme"} {
		t.Run(issuer, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw := map[string]any{"kubeContext": "dev", "namespace": "ns", "registry": "registry.test", "ingressHost": "app.dev.lab", "publicHosts": []string{"app.dev.lab", "account.dev.lab"}}
			if issuer != "" {
				raw["clusterIssuer"] = issuer
			}
			b, _ := json.Marshal(raw)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			ds := []config.Deployable{
				{Name: "web", PublicPath: "/", Port: 3000, ReadinessPath: "/", BaseService: config.ObjectRef{Name: "web", Namespace: "ns", Port: 3000}},
				{Name: "api", PublicPath: "/api/internal", InternalHost: "api.internal", Port: 8000, ReadinessPath: "/readyz", PublicProxy: config.ObjectRef{Name: "proxy", Namespace: "ns", Port: 4455}, BaseService: config.ObjectRef{Name: "api", Namespace: "ns", Port: 8000}},
				{Name: "auth", PublicPath: "/", PublicHost: "account.dev.lab", Port: 3000, ReadinessPath: "/", BaseService: config.ObjectRef{Name: "auth", Namespace: "ns", Port: 3000}},
			}
			r := &captureRunner{}
			route := "alice-feature-abcd1234"
			err = (Client{Run: r, Context: "dev", Namespace: "ns"}).ApplyRoutes(context.Background(), cfg, ds, map[string]bool{"web": true, "api": true}, "alice", "feature", "head", "main", "base", route, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var list struct{ Items []map[string]any }
			if err := json.Unmarshal(r.payload, &list); err != nil {
				t.Fatal(err)
			}
			objects := map[string]map[string]any{}
			for _, item := range list.Items {
				objects[item["metadata"].(map[string]any)["name"].(string)] = item
			}
			for _, d := range ds {
				hostRoute := objects[naming.Resource("route-host-"+d.Name, route)]
				if issuer == "" {
					if hostRoute != nil {
						t.Fatal("unexpected host route")
					}
					continue
				}
				if hostRoute == nil {
					t.Fatal("missing host route", d.Name)
				}
				spec := hostRoute["spec"].(map[string]any)
				if !reflect.DeepEqual(spec["hostnames"], []any{route + "." + d.Host(cfg)}) {
					t.Fatal("wrong hostname", spec)
				}
				rules := spec["rules"].([]any)
				if len(rules) != 1 {
					t.Fatal("host route must have one rule")
				}
				rule := rules[0].(map[string]any)
				matches := rule["matches"].([]any)
				if len(matches) != 1 || len(matches[0].(map[string]any)) != 1 {
					t.Fatal("host route requires only path matching", matches)
				}
				cookieName := naming.Resource("route-"+d.Name, route)
				if d.InternalHost != "" {
					cookieName = naming.Resource("route-public-"+d.Name, route)
				}
				cookieRule := objects[cookieName]["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
				if !reflect.DeepEqual(rule["backendRefs"], cookieRule["backendRefs"]) || !reflect.DeepEqual(rule["filters"], cookieRule["filters"]) {
					t.Fatal("host bypasses existing routing or auth", rule)
				}
			}
			ingress := objects[naming.Resource("hosts", route)]
			if issuer == "" {
				if ingress != nil {
					t.Fatal("unexpected ingress")
				}
				return
			}
			if ingress == nil {
				t.Fatal("missing ingress")
			}
			spec := ingress["spec"].(map[string]any)
			tls := spec["tls"].([]any)[0].(map[string]any)
			if !reflect.DeepEqual(tls["hosts"], []any{route + ".app.dev.lab", route + ".account.dev.lab"}) || tls["secretName"] != naming.Resource("tls", route) {
				t.Fatal("wrong certificate", tls)
			}
			meta := ingress["metadata"].(map[string]any)
			if meta["annotations"].(map[string]any)["cert-manager.io/cluster-issuer"] != issuer {
				t.Fatal("wrong issuer")
			}
			if meta["labels"].(map[string]any)[OwnerLabel] != "alice" {
				t.Fatal("unowned ingress")
			}
		})
	}
}
