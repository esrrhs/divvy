package tools

import (
	"os"
	"strings"
)

// isolatedEnvAllow is the only host environment names that may be inherited by
// a sandboxed command. Everything else (tokens, sentinels, user secrets, home
// paths) is dropped. Tool caches are provided as read-only paths or redirected
// into a scratch directory, not by exposing the host home directory.
//
// Shared by the Linux (bubblewrap) and macOS (sandbox-exec) backends so the two
// leak the same set of variables.
var isolatedEnvAllow = map[string]bool{
	"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
	"LC_MESSAGES": true, "LC_COLLATE": true, "LC_NUMERIC": true,
	"LC_TIME": true, "LC_MONETARY": true, "TZ": true, "TERM": true,
}

// filterIsolatedEnv keeps only the allow-listed host variables. A nil env means
// "inherit the current process"; the result is always freshly allocated so
// callers can append to it.
func filterIsolatedEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, 16)
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if isolatedEnvAllow[name] || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}
