package config

import (
	"fmt"
	"os"
	"path/filepath"
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
	if c.GatewayNamespace != "ns" {
		t.Fatalf("gateway namespace = %q", c.GatewayNamespace)
	}
}
