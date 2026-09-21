package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectInvalidCookieAndTTL(t *testing.T) {
	for _, tc := range []struct{ cookie, ttl string }{{"bad;cookie", "8h"}, {"dev_branch", "0s"}, {"dev_branch", "500ms"}, {"dev_branch", "invalid"}} {
		p := filepath.Join(t.TempDir(), "config.json")
		data := fmt.Sprintf(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test","cookieName":%q,"ttl":%q}`, tc.cookie, tc.ttl)
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TTL != "8h" || c.MetadataQuery == "" || c.CookieName != "dev_branch" || c.GatewayName != "dev-cli-playground" {
		t.Fatalf("defaults missing: %#v", c)
	}
	if c.GatewayNamespace != "ns" || c.TaskLockNamespace != "ns" || c.InternalGateway != "http://dev-cli-gateway.ns.svc.cluster.local" || len(c.Namespaces) != 1 || len(c.PublicHosts) != 0 {
		t.Fatalf("gateway namespace = %q", c.GatewayNamespace)
	}
}

func TestDiscoverYAMLAndLegacyJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".dev", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err == nil {
		t.Fatal("missing config accepted")
	}
	if err := os.WriteFile(".dev/config.json", []byte(`{"kubeContext":"json","namespace":"ns","registry":"registry.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(""); err != nil || got.KubeContext != "json" {
		t.Fatalf("JSON fallback = %+v, %v", got, err)
	}
	yaml := "kubeContext: yaml\nnamespace: ns\nregistry: registry.test\nbazelArgs: [--config=agent]\n"
	if err := os.WriteFile(".dev/config.yaml", []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("ambiguous config = %v", err)
	}
	if got, err := Load(".dev/config.yaml"); err != nil || got.KubeContext != "yaml" || len(got.BazelArgs) != 1 {
		t.Fatalf("explicit YAML = %+v, %v", got, err)
	}
	if err := os.Remove(".dev/config.json"); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(""); err != nil || got.KubeContext != "yaml" {
		t.Fatalf("YAML discovery = %+v, %v", got, err)
	}
}

func TestDependencyAndTaskValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	valid := `{"kubeContext":"dev","namespace":"apps","namespaces":["apps","data"],"registry":"registry.test","dependencies":[{"name":"postgres","namespace":"data","manifest":"deps/postgres.json","readiness":[{"resource":"postgresql","name":"db-${DEV_ROUTE}","jsonPath":".status.PostgresClusterStatus","value":"Running"}]}],"tasks":[{"name":"migrate","namespace":"apps","manifest":"tasks/migrate.json","target":"postgres.data","dependencies":["postgres"]}]}`
	if err := os.WriteFile(p, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dependencies[0].Retention != "retain" || c.Tasks[0].Timeout != "5m" || c.TaskLockNamespace != "apps" {
		t.Fatalf("defaults: %#v", c)
	}
	for _, replacement := range []string{`"jsonPath":"","value":"Running"`, `"dependencies":["missing"]`} {
		bad := valid
		if strings.HasPrefix(replacement, `"jsonPath`) {
			bad = strings.Replace(valid, `"jsonPath":".status.PostgresClusterStatus","value":"Running"`, replacement, 1)
		} else {
			bad = strings.Replace(valid, `"dependencies":["postgres"]`, replacement, 1)
		}
		if err := os.WriteFile(p, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", replacement)
		}
	}
}
