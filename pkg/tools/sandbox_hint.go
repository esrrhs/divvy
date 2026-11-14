package tools

import "strings"

// sandboxWriteHint explains a write the sandbox refused outside the workspace.
//
// The raw kernel text ("mkdir /tmp/: operation not permitted") reads like a bug
// in the project under test, and that is how it gets treated: a real run spent
// ten tool calls hunting git, then xcode-select, then sudo (refused by the very
// sandbox it was fighting) after `go build -o /tmp/app` was denied. Naming the
// writable alternative is the whole fix — the policy does not have to move.
const sandboxWriteHint = "[sandbox] this command was refused a write outside the workspace. " +
	"The only writable places are the workspace itself and \"$TMPDIR\"; a bare /tmp is not " +
	"necessarily that directory (on macOS it is not writable at all). " +
	"Retry with \"$TMPDIR/...\" or a path inside the workspace."

// SandboxDenialHint returns an explanation to append to a failed command's
// output, or "" when the failure is not a sandbox write refusal.
//
// Only unambiguous refusal markers are matched. "Permission denied" is
// deliberately NOT one of them: it is an ordinary EACCES a project can produce
// on its own (chmod, an unreadable fixture), and sandbox advice there would
// send the model hunting in the wrong direction — the same mistake as reporting
// "应该成立" as "已经验证".
func SandboxDenialHint(stdout, stderr string) string {
	blob := stderr + "\n" + stdout
	for _, m := range []string{
		"Operation not permitted",
		"operation not permitted",
		"Read-only file system",
		"read-only file system",
	} {
		if strings.Contains(blob, m) {
			return sandboxWriteHint
		}
	}
	return ""
}
