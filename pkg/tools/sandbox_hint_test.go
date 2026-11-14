package tools

import (
	"strings"
	"testing"
)

// The marker list is the whole discrimination: too narrow and a refused write
// stays unreadable, too broad and ordinary permission problems get sandbox
// advice. Both mistakes have a cost measured in tool calls.
func TestSandboxDenialHint_Markers(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		wantHint bool
	}{
		{
			name:     "seatbelt write refusal",
			stdout:   "mkdir: /tmp/config_cli: operation not permitted\n",
			wantHint: true,
		},
		{
			name:     "capitalised coreutils wording",
			stdout:   "mkdir: cannot create directory '/tmp/x': Operation not permitted",
			wantHint: true,
		},
		{
			name:     "bwrap ro-bind inside the sandbox",
			stdout:   "sh: 1: cannot create /etc/x: Read-only file system",
			wantHint: true,
		},
		{
			name:     "denial only on stderr",
			stderr:   "cp: cannot create regular file '/Users/x/y': Operation not permitted",
			wantHint: true,
		},
		// "Permission denied" is an ordinary EACCES a project can produce by
		// itself. Advising "$TMPDIR" there sends the model looking for a
		// sandbox problem that does not exist.
		{
			name:   "plain EACCES is not a sandbox denial",
			stderr: "chmod: /workspace/locked.txt: Permission denied",
		},
		{
			name:   "ordinary build failure",
			stderr: "./main.go:4:2: undefined: nope\n",
		},
		{
			name: "no output at all",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SandboxDenialHint(tc.stdout, tc.stderr)
			if tc.wantHint != (got != "") {
				t.Fatalf("hint = %q, want hint = %v", got, tc.wantHint)
			}
			if !tc.wantHint {
				return
			}
			// The hint is worthless if it does not name a writable alternative:
			// "you were refused" is exactly what the kernel already said.
			for _, want := range []string{"$TMPDIR", "workspace"} {
				if !strings.Contains(got, want) {
					t.Fatalf("hint does not mention %q: %q", want, got)
				}
			}
		})
	}
}
