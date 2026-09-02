package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TTL != "8h" || c.MetadataQuery == "" || c.CookieName != "dev_branch" {
		t.Fatalf("defaults missing: %#v", c)
	}
}
