package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBranchHostsRequireIssuer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || err.Error() != "clusterIssuer is required: branch hosts are the only branch selection" {
		t.Fatalf("missing issuer: %v", err)
	}
}
