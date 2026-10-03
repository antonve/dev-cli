// Package deeplink defines the gateway-owned browser selection contract.
package deeplink

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

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
	u.Scheme, u.Host, u.RawQuery = "https", route+"."+host, query.Encode()
	return u.String(), nil
}
