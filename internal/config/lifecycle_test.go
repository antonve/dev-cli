package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsUnknownConfigKeys(t *testing.T) {
	for _, extension := range []string{`,"futureLifecycle":true`, `,"tasks":[{"name":"tenant","namespace":"ns","target":"db","manifest":"task.yaml","futureTask":true}]`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test"`+extension+`}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown key accepted: %v", err)
		}
	}
}

func TestLoadRejectsUndeclaredHookTasks(t *testing.T) {
	for _, hooks := range []string{`{"beforeUp":["missing"]}`, `{"afterDown":["missing"]}`, `{"deployables":{"worker":{"beforeStart":["missing"]}}}`, `{"deployables":{"worker":{"afterStop":["missing"]}}}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test","hooks":`+hooks+`}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("undeclared hook task accepted")
		}
	}
}
