package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

func Slug(s string, limit int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
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
	if len(name) <= 63 {
		return name
	}
	return strings.Trim(name[:63], "-")
}
