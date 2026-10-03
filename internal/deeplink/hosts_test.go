package deeplink

import "testing"

func TestHostURL(t *testing.T) {
	got, err := HostURL("app.dev.lab", "alice-x-1a2b3c4d", "/settings?tab=profile&dev-branch=old#details")
	if err != nil || got != "https://alice-x-1a2b3c4d.app.dev.lab/settings?tab=profile#details" {
		t.Fatalf("%s %v", got, err)
	}
	for _, route := range []string{"", "-x", "x-", "a.b", "UPPER", "a/b"} {
		if _, err := HostURL("app.dev.lab", route, "/"); err == nil {
			t.Fatal("accepted route", route)
		}
	}
	for _, host := range []string{"", "user@app.dev.lab", "app.dev.lab/path", "app.dev.lab?x=y", "app.dev.lab#fragment", "app.dev.lab\\evil", "localhost:3000", "127.0.0.1"} {
		if _, err := HostURL(host, "route", "/"); err == nil {
			t.Fatal("accepted host", host)
		}
	}
	for _, path := range []string{"https://evil.test/", "//evil.test/", "relative", "/\\evil.test", "/%2f/evil.test", "/?q=%XX", "/\r\n"} {
		if _, err := HostURL("app.dev.lab", "route", path); err == nil {
			t.Fatal("accepted destination", path)
		}
	}
}
