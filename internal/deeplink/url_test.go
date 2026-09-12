package deeplink

import (
	"net/url"
	"testing"
)

func TestURLPreservesDeepDestinationAndReplacesSelection(t *testing.T) {
	for _, selection := range []string{"alice-feature-abc", Base} {
		link, err := URL("app.dev.lab", "/settings?tab=profile&tag=a&tag=b&dev-branch=old&dev-branch=older#section", selection)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(link)
		if u.Scheme != "https" || u.Host != "app.dev.lab" || u.Path != "/settings" || u.Fragment != "section" || u.Query().Get("tab") != "profile" || len(u.Query()["tag"]) != 2 || len(u.Query()[Parameter]) != 1 || u.Query().Get(Parameter) != selection {
			t.Fatalf("lost destination or selection: %s", link)
		}
	}
	got, err := URL("app.dev.lab", "", "route")
	if err != nil || got != "https://app.dev.lab/?dev-branch=route" {
		t.Fatalf("%s %v", got, err)
	}
}

func TestURLRejectsExternalAndMalformedDestinations(t *testing.T) {
	for _, path := range []string{"https://evil.test/", "//evil.test/", "relative", "/\\evil.test", "/%2f/evil.test", "/?q=%XX", "/\r\n"} {
		if got, err := URL("app.dev.lab", path, "route"); err == nil {
			t.Fatalf("accepted %q: %s", path, got)
		}
	}
	for _, host := range []string{"", "user@app.dev.lab", "app.dev.lab/path", "app.dev.lab?x=y", "app.dev.lab#fragment", "app.dev.lab\\evil"} {
		if got, err := URL(host, "/", "route"); err == nil {
			t.Fatalf("accepted host %q: %s", host, got)
		}
	}
}
