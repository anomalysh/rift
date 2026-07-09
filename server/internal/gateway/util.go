package gateway

import (
	"encoding/json"
	"strings"
)

func unmarshalFrame(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}

// lowerASCII lowercases an HTTP header name. Header names are ASCII by
// definition, so strings.ToLower's Unicode machinery is unnecessary here.
func lowerASCII(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func splitAndTrim(s string, sep byte) []string {
	parts := strings.Split(s, string(sep))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
