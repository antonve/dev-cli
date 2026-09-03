package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDestination(t *testing.T) {
	repository, tagged, err := Destination("registry.test/team", "hello-api", "route-deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if repository != "registry.test/team/hello-api" || tagged != repository+":route-deadbeef" {
		t.Fatalf("%q %q", repository, tagged)
	}
}

func TestResolve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/hello/manifests/latest" || !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image") {
			t.Fatalf("unexpected request %s %#v", r.URL.Path, r.Header)
		}
		w.Header().Set("Docker-Content-Digest", "sha256:abcdef")
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer server.Close()

	resolved, err := (Resolver{Client: server.Client()}).Resolve(context.Background(), server.URL+"/team/hello:latest")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimPrefix(server.URL, "http://") + "/team/hello@sha256:abcdef"
	if resolved != want {
		t.Fatalf("got %q want %q", resolved, want)
	}
}
