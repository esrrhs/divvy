package server

import "strings"

// redaction replaces secret material in any outbound/logged text.
const redaction = "***REDACTED***"

// Redact replaces every occurrence of each non-trivial secret in s with a
// fixed marker, so logs and SSE payloads can echo tool output without
// leaking credentials. Empty/short values are ignored to avoid wiping
// single characters (a 1-2 char key is not a real secret).
func Redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, redaction)
	}
	return s
}

// RedactBytes is the byte-payload variant used on the SSE write path.
func RedactBytes(data []byte, secrets ...string) []byte {
	if len(secrets) == 0 {
		return data
	}
	return []byte(Redact(string(data), secrets...))
}
