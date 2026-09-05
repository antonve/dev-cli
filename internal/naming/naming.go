package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func Slug(s string, limit int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		result = "dev"
	}
	if len(result) > limit {
		result = strings.Trim(result[:limit], "-")
	}
	return result
}

func RouteKey(owner, branch string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + branch))
	suffix := hex.EncodeToString(h[:])[:8]
	prefix := Slug(owner, 12) + "-" + Slug(branch, 22)
	if len(prefix) > 38 {
		prefix = strings.Trim(prefix[:38], "-")
	}
	return prefix + "-" + suffix
}

func Resource(service, route string) string {
	name := Slug(service, 20) + "-dev-" + route
	// Reserve eight characters for the runtime ConfigMap suffix. Hash the
	// original inputs whenever truncation or normalization could lose identity.
	if len(name) <= 55 && service == Slug(service, 20) {
		return name
	}
	h := sha256.Sum256([]byte(service + "\x00" + route))
	return Slug(name, 42) + "-" + hex.EncodeToString(h[:6])
}
