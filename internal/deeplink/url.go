// Package deeplink defines the gateway-owned browser selection contract.
package deeplink

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

const Parameter = "dev-branch"
const Base = "base"

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func HostURL(host, route, destination string) (string, error) {
	if !dnsLabel.MatchString(route) || net.ParseIP(host) != nil || len(host)+len(route)+1 > 253 {
		return "", fmt.Errorf("invalid branch host")
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(label) {
			return "", fmt.Errorf("invalid public host %q", host)
		}
	}
	link, err := URL(route+"."+host, destination, route)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(link)
	query := u.Query()
	query.Del(Parameter)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

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
