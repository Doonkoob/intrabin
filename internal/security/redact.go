package security

import (
	"regexp"
	"strings"
)

var sensitiveKeyRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|authorization|cookie|private[_-]?key|credential)`)

// IsSensitiveKey reports whether a header/field name likely carries a secret.
func IsSensitiveKey(k string) bool { return sensitiveKeyRe.MatchString(k) }

// RedactMap returns a copy of m with sensitive values replaced.
func RedactMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if IsSensitiveKey(k) {
			out[k] = "[REDACTED]"
		} else {
			out[k] = v
		}
	}
	return out
}

var urlUserinfoRe = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

// RedactString removes credentials embedded in URLs and truncates long messages.
func RedactString(s string) string {
	s = urlUserinfoRe.ReplaceAllString(s, "${1}[REDACTED]@")
	if len(s) > 1000 {
		s = s[:1000] + "…"
	}
	return strings.ToValidUTF8(s, "")
}
