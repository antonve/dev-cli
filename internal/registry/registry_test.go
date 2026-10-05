package registry

import (
	"context"
	"errors"
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

func TestResolveAnswersAnonymousBearerChallenge(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.URL.Query().Get("service") != "registry.test" || r.URL.Query().Get("scope") != "repository:team/hello:pull" {
				t.Fatalf("token request %s", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "" {
				t.Fatal("anonymous token request sent credentials")
			}
			_, _ = w.Write([]byte(`{"token":"anonymous"}`))
		case "/v2/team/hello/manifests/route-abc":
			if r.Header.Get("Authorization") != "Bearer anonymous" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:team/hello:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:abcdef")
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolved, err := (Resolver{Client: server.Client()}).Resolve(context.Background(), server.URL+"/team/hello:route-abc")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimPrefix(server.URL, "http://") + "/team/hello@sha256:abcdef"; resolved != want {
		t.Fatalf("got %q want %q", resolved, want)
	}
}

func TestResolveReportsDeniedAnonymousToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED"}]}`))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:team/private:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := (Resolver{Client: server.Client()}).Resolve(context.Background(), server.URL+"/team/private:route-abc")
	if !errors.Is(err, ErrNotPullable) || !strings.Contains(err.Error(), "team/private") {
		t.Fatalf("denied token: %v", err)
	}
}

func TestResolveReportsMissingTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := (Resolver{Client: server.Client()}).Resolve(context.Background(), server.URL+"/team/hello:route-abc")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
}
