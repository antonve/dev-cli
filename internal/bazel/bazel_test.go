package bazel

import "testing"

func TestLabelPath(t *testing.T) {
	tests := map[string]string{"//apps/api:main.go": "apps/api/main.go", "//:MODULE.bazel": "MODULE.bazel"}
	for in, want := range tests {
		got, ok := labelPath(in)
		if !ok || got != want {
			t.Fatalf("%s = %q,%v", in, got, ok)
		}
	}
}

func TestConfiguredTarget(t *testing.T) {
	if got, want := configuredTarget("//apps/api:api"), "config(//apps/api:api, target)"; got != want {
		t.Fatalf("configuredTarget() = %q, want %q", got, want)
	}
}
