package tools

import "testing"

// The finish gate BLOCKS on a secret finding, so a shape-based match on a
// placeholder makes the model rewrite the user's own example — work nobody
// asked for, on every turn that touches the file.
func TestSecretGateIgnoresPlaceholdersButCatchesRealKeys(t *testing.T) {
	placeholders := []string{
		`password: "changeme"`,
		`token: "YOUR_TOKEN_HERE"`,
		`api_key = "example-key"`,
		`secret: "placeholder-value"`,
		`passwd: "sample-password"`,
	}
	for _, line := range placeholders {
		if secretLooksReal(line) {
			t.Errorf("%q is a placeholder, not a leaked credential", line)
		}
	}
	reals := []string{
		`api_key = "sk-live-9f2aQ1xZ"`,
		`password: "Xq7#vTn2Lp"`,
		`token: "ghp_ABCdef1234567890"`,
	}
	for _, line := range reals {
		if !secretLooksReal(line) {
			t.Errorf("%q looks like a real credential and must be flagged", line)
		}
	}
}
