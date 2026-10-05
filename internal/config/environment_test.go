package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const environmentBase = "kubeContext: prod\nnamespace: routing\nregistry: registry.test/team\n"

func TestDeployableOverridesReplaceEnvironmentFields(t *testing.T) {
	cfg := Config{Deployables: map[string]DeployableOverride{"api": {
		Namespace:        "prod-api",
		WorkloadTemplate: "api.yaml",
		BaseService:      ObjectRef{Name: "tadoku-api", Namespace: "prod-api", Port: 80},
		PublicProxy:      ObjectRef{Name: "proxy", Namespace: "prod-auth", Port: 4455},
		InternalHost:     "api.internal.prod",
		PublicHost:       "preview.example",
	}}}
	api := Deployable{
		Name:             "api",
		Namespace:        "dev-api",
		WorkloadTemplate: ".dev/api.yaml",
		BaseService:      ObjectRef{Name: "tadoku-api", Namespace: "dev-api", Port: 80},
		PublicProxy:      ObjectRef{Name: "proxy", Namespace: "dev-auth", Port: 4455},
		InternalHost:     "api.internal.dev",
		PublicHost:       "dev.example",
		Port:             8000,
	}
	web := Deployable{Name: "web", Namespace: "dev-web", Port: 3000}

	got, err := cfg.ApplyOverrides([]Deployable{api, web})
	if err != nil {
		t.Fatal(err)
	}
	want := api
	want.Namespace, want.WorkloadTemplate, want.InternalHost, want.PublicHost = "prod-api", "api.yaml", "api.internal.prod", "preview.example"
	want.BaseService = ObjectRef{Name: "tadoku-api", Namespace: "prod-api", Port: 80}
	want.PublicProxy = ObjectRef{Name: "proxy", Namespace: "prod-auth", Port: 4455}
	if !reflect.DeepEqual(got, []Deployable{want, web}) {
		t.Fatalf("overrides:\n got %#v\nwant %#v", got, []Deployable{want, web})
	}
}

func TestUnknownDeployableOverrideFails(t *testing.T) {
	cfg := Config{Deployables: map[string]DeployableOverride{"missing": {Namespace: "x"}}}
	if _, err := cfg.ApplyOverrides([]Deployable{{Name: "api"}}); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("unknown override accepted: %v", err)
	}
}

func TestManifestsRelativeToConfig(t *testing.T) {
	path := writeConfig(t, environmentBase+"clusterIssuer: ca\nmanifestsRelativeTo: config\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(filepath.Dir(path))
	if cfg.ManifestRoot != dir {
		t.Fatalf("manifest root %q, want %q", cfg.ManifestRoot, dir)
	}

	cfg, err = Load(writeConfig(t, environmentBase+"clusterIssuer: ca\n"))
	if err != nil || cfg.ManifestRoot != "" {
		t.Fatalf("repository default: %q %v", cfg.ManifestRoot, err)
	}
	if _, err := Load(writeConfig(t, environmentBase+"clusterIssuer: ca\nmanifestsRelativeTo: home\n")); err == nil {
		t.Fatal("unknown manifestsRelativeTo accepted")
	}
}

func TestGatewayOwnedHostTLSNeedsNoIssuer(t *testing.T) {
	cfg, err := Load(writeConfig(t, environmentBase+"hostTLS: gateway\npublicHosts: [preview.example]\n"))
	if err != nil || cfg.HostTLS != HostTLSGateway {
		t.Fatalf("gateway TLS: %#v %v", cfg.HostTLS, err)
	}
	cfg, err = Load(writeConfig(t, environmentBase+"clusterIssuer: ca\n"))
	if err != nil || cfg.HostTLS != HostTLSCertificate {
		t.Fatalf("certificate default: %#v %v", cfg.HostTLS, err)
	}
	if _, err := Load(writeConfig(t, environmentBase+"hostTLS: wildcard\n")); err == nil {
		t.Fatal("unknown hostTLS accepted")
	}
}

func TestInternalGatewayDefaultsToPublicGateway(t *testing.T) {
	cfg, err := Load(writeConfig(t, environmentBase+"clusterIssuer: ca\ngatewayName: edge\ngatewayNamespace: gw\n"))
	if err != nil || cfg.InternalGatewayName != "edge" || cfg.InternalGatewayNamespace != "gw" {
		t.Fatalf("internal gateway default: %q/%q %v", cfg.InternalGatewayNamespace, cfg.InternalGatewayName, err)
	}
	cfg, err = Load(writeConfig(t, environmentBase+"clusterIssuer: ca\ninternalGatewayName: internal\ninternalGatewayNamespace: routing\n"))
	if err != nil || cfg.InternalGatewayName != "internal" || cfg.InternalGatewayNamespace != "routing" {
		t.Fatalf("internal gateway: %q/%q %v", cfg.InternalGatewayNamespace, cfg.InternalGatewayName, err)
	}
}
