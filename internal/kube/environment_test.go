package kube

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

func TestConfigRelativeManifestsCannotEscapeTheirRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "api.yaml"), []byte("spec:\n  containers: [{name: api}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("spec: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(root, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	if _, err := loadPodTemplate(root, "api.yaml", "route", "ns", nil); err != nil {
		t.Fatalf("config-relative template: %v", err)
	}
	if _, err := loadPodTemplate("", "api.yaml", "route", "ns", nil); err == nil {
		t.Fatal("repository-relative lookup read the config directory")
	}
	for _, path := range []string{"link.yaml", "../secret.yaml", filepath.Join(outside, "secret.yaml")} {
		if _, err := loadPodTemplate(root, path, "route", "ns", nil); err == nil {
			t.Fatalf("%s escaped the manifest root", path)
		}
	}
}

func TestGatewayOwnedTLSAndInternalGatewayParents(t *testing.T) {
	cfg := config.Config{
		Namespace:                "branches",
		IngressHost:              "preview.example",
		PublicHosts:              []string{"preview.example"},
		HostTLS:                  config.HostTLSGateway,
		GatewayName:              "edge",
		GatewayNamespace:         "gateway",
		InternalGatewayName:      "internal",
		InternalGatewayNamespace: "routing",
	}
	d := config.Deployable{
		Name:          "api",
		PublicPath:    "/api",
		InternalHost:  "api.internal",
		Port:          8000,
		ReadinessPath: "/readyz",
		BaseService:   config.ObjectRef{Name: "api", Namespace: "apps", Port: 80},
		PublicProxy:   config.ObjectRef{Name: "proxy", Namespace: "auth", Port: 4455},
	}
	r := &captureRunner{}
	route := "alice-feature-abcd1234"
	err := (Client{Run: r, Context: "prod", Namespace: "branches"}).ApplyRoutes(
		context.Background(),
		cfg,
		[]config.Deployable{d},
		map[string]bool{"api": true},
		"alice",
		"feature",
		"head",
		"main",
		"base",
		route,
		time.Now().Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}

	var list struct {
		Items []struct {
			Kind     string
			Metadata struct{ Name string }
			Spec     struct {
				ParentRefs []struct{ Name, Namespace string }
				Rules      []struct {
					Filters []struct {
						ResponseHeaderModifier struct {
							Set []struct{ Name, Value string }
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(r.payload, &list); err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	for _, item := range list.Items {
		if item.Kind == "Ingress" {
			t.Fatal("gateway-owned TLS must not create a per-branch Ingress or certificate")
		}
		if item.Kind != "HTTPRoute" {
			continue
		}
		parent := item.Spec.ParentRefs[0]
		parents[item.Metadata.Name] = parent.Namespace + "/" + parent.Name
		if item.Metadata.Name != naming.Resource("route-host-api", route) {
			continue
		}
		set := item.Spec.Rules[0].Filters[1].ResponseHeaderModifier.Set
		if !slices.Contains(set, struct{ Name, Value string }{"X-Robots-Tag", "noindex, nofollow"}) {
			t.Fatalf("branch host response is indexable: %v", set)
		}
	}
	if got := parents[naming.Resource("route-api", route)]; got != "routing/internal" {
		t.Fatalf("internal data route parent %q", got)
	}
	if got := parents[naming.Resource("route-host-api", route)]; got != "gateway/edge" {
		t.Fatalf("public host route parent %q", got)
	}
	if strings.Contains(string(r.payload), "cert-manager.io") {
		t.Fatal("gateway-owned TLS requested a certificate")
	}
}
