package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintChanges(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "app.tsx")
	if err := os.WriteFile(p, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := fingerprint([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := fingerprint([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("fingerprint did not change")
	}
}
