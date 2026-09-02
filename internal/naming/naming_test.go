package naming

import "testing"

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
