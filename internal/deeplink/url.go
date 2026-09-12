// Package deeplink defines the gateway-owned browser selection contract.
package deeplink

import (
	"fmt"
	"net/url"
	"strings"
)

const Parameter = "dev-branch"
const Base = "base"

// URL accepts only a root-relative destination on the configured application
// host. It replaces selection parameters and preserves other query values and
// fragments; it never creates a link to a caller-supplied external host.
func URL(host, destination, selection string) (string, error) {
	origin, err := url.Parse("https://" + host)
	if err != nil || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || strings.ContainsAny(host, "\\?#") {
		return "", fmt.Errorf("invalid ingressHost %q", host)
	}
	if destination == "" {
		destination = "/"
	}
	u, err := url.Parse(destination)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(destination, "\\\r\n") {
		return "", fmt.Errorf("destination must be a root-relative application path")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid destination query: %w", err)
	}
	query.Set(Parameter, selection)
	u.Scheme, u.Host, u.RawQuery = origin.Scheme, origin.Host, query.Encode()
	return u.String(), nil
}
