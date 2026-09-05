package naming

import (
	"regexp"
	"strings"
	"testing"
)

func TestRouteKeyStableAndDistinct(t *testing.T) {
	a := RouteKey("Alice@example.com", "Feature/X")
	b := RouteKey("Bob@example.com", "Feature/X")
	if a != RouteKey("Alice@example.com", "Feature/X") {
		t.Fatal("route key not stable")
	}
	if a == b {
		t.Fatal("owners collided")
	}
	if len(Resource("hello-api", a)) > 63 {
		t.Fatal("resource name too long")
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("Feature/One_TWO", 30); got != "feature-one-two" {
		t.Fatalf("got %q", got)
	}
}

func TestNamesRemainValidAndDistinctAfterNormalization(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	seen := map[string]bool{}
	for _, owner := range []string{"ユーザー", "équipe", "alice@example.com"} {
		for _, branch := range []string{"功能/改进", strings.Repeat("a", 80) + "1", strings.Repeat("a", 80) + "2"} {
			route := RouteKey(owner, branch)
			if !valid.MatchString(route) || len(route) > 63 {
				t.Fatalf("invalid route %q", route)
			}
			for _, service := range []string{strings.Repeat("s", 40) + "1", strings.Repeat("s", 40) + "2", "hello-api"} {
				name := Resource(service, route)
				if !valid.MatchString(name) || len(name+"-runtime") > 63 || seen[name] {
					t.Fatalf("invalid or colliding name %q", name)
				}
				seen[name] = true
			}
		}
	}
}
