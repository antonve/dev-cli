package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProfilePrefixMatchingAndFirstMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"kubeContext":"dev","namespace":"ns","registry":"registry.test","variables":{"DATABASE":"base","LOCATION":"${DEV_NAMESPACE}/${DEV_ROUTE}"},"tasks":[{"name":"tenant","namespace":"ns","target":"db","manifest":"task.yaml"},{"name":"migrate","namespace":"ns","target":"db","manifest":"task.yaml"},{"name":"after","namespace":"ns","target":"db","manifest":"task.yaml"}],"hooks":{"beforeUp":["tenant"],"afterDown":["after"],"deployables":{"worker":{"beforeStart":["tenant"],"afterStop":["after"]}}},"profiles":[{"name":"isolated","whenChanged":["migrations/"],"variables":{"DATABASE":"tadoku-${DEV_ROUTE}"},"hooks":{"beforeUp":["migrate","tenant"],"deployables":{"worker":{"beforeStart":[]}}}},{"name":"later","whenChanged":["migrations/"],"variables":{"DATABASE":"later"}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		changed           []string
		profile, database string
	}{{nil, "default", "base"}, {[]string{"migrations-old/001.sql"}, "default", "base"}, {[]string{"migrations/001.sql"}, "isolated", "tadoku-route"}} {
		effective, profile := cfg.ForPaths(tc.changed)
		vars := effective.ResolveVariables("route", "ns")
		if profile != tc.profile || vars["DATABASE"] != tc.database || vars["LOCATION"] != "ns/route" {
			t.Fatalf("profile=%s vars=%v", profile, vars)
		}
		if !reflect.DeepEqual(effective.Hooks.AfterDown, []string{"after"}) || !reflect.DeepEqual(effective.Hooks.Deployables["worker"].AfterStop, []string{"after"}) {
			t.Fatal("profile lost unspecified hooks")
		}
		if profile == "isolated" && (len(effective.Hooks.Deployables["worker"].BeforeStart) != 0 || !reflect.DeepEqual(effective.Hooks.BeforeUp, []string{"migrate", "tenant"})) {
			t.Fatalf("hook replacement: %+v", effective.Hooks)
		}
	}
	if cfg.Variables["DATABASE"] != "base" || !reflect.DeepEqual(cfg.Hooks.Deployables["worker"].BeforeStart, []string{"tenant"}) {
		t.Fatal("profile mutated top-level config")
	}
}
func TestProfileAndVariableValidation(t *testing.T) {
	for _, extra := range []string{
		`,"variables":{"bad":"value"}`,
		`,"profiles":[{"name":"same","whenChanged":["a/"]},{"name":"same","whenChanged":["b/"]}]`,
		`,"profiles":[{"name":"","whenChanged":["a/"]}]`,
		`,"profiles":[{"name":"default","whenChanged":["a/"]}]`,
		`,"profiles":[{"name":"bad","whenChanged":[]}]`,
		`,"profiles":[{"name":"bad","whenChanged":[""]}]`,
		`,"profiles":[{"name":"bad","whenChanged":["../outside/"]}]`,
		`,"profiles":[{"name":"bad","whenChanged":["/absolute/"]}]`,
		`,"profiles":[{"name":"bad","whenChanged":["a/"],"variables":{"DATABASE":"value"}}]`,
		`,"profiles":[{"name":"bad","whenChanged":["a/"],"hooks":{"beforeUp":["missing"]}}]`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test"`+extra+`}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid profile/variable accepted: %s", extra)
		}
	}
}
func TestUnknownVariableInVariableValueRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"kubeContext":"dev","namespace":"ns","registry":"registry.test","variables":{"DATABASE":"${DEV_VAR_OTHER}"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "DEV_VAR") {
		t.Fatalf("recursive/unknown variable accepted: %v", err)
	}
}
