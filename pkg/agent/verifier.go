package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// VerifyResult is the outcome of running Definition of Done commands.
type VerifyResult struct {
	OK       bool
	Command  string
	Output   string
	ExitCode int
}

func (o *Orchestrator) verify(ctx context.Context, sb *tools.Sandbox, node *models.TaskNode) VerifyResult {
	cmds := node.DoD.Commands
	if len(cmds) == 0 {
		ensureDoD(&node.DoD, node.Contract.Outputs, tools.DetectProject(sb.Root))
		cmds = node.DoD.Commands
	}
	timeout := time.Duration(node.DoD.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	var combined strings.Builder
	for _, cmd := range cmds {
		o.log.Actionf("verify: %s", cmd)
		start := time.Now()
		res, err := sb.RunBash(ctx, cmd, timeout)
		duration := time.Since(start).Milliseconds()
		if err != nil {
			msg := fmt.Sprintf("command %q failed to start: %v", cmd, err)
			o.log.Errorf("%s", msg)
			o.events.Record("verify", node.ID, map[string]any{
				"command": cmd, "ok": false, "exit_code": -1,
				"duration_ms": duration, "error": clip(err.Error(), evReasonChars),
			})
			return VerifyResult{OK: false, Command: cmd, Output: msg, ExitCode: -1}
		}
		fmt.Fprintf(&combined, "$ %s\nexit %d\n%s\n%s\n", cmd, res.ExitCode, res.Stdout, res.Stderr)
		if res.TimedOut {
			msg := combined.String() + "timed out\n"
			o.log.Errorf("verify timeout: %s", cmd)
			o.events.Record("verify", node.ID, map[string]any{
				"command": cmd, "ok": false, "exit_code": res.ExitCode,
				"timed_out":   true,
				"duration_ms": duration,
				"output_tail": tailClip(combined.String(), evVerifyTail),
			})
			return VerifyResult{OK: false, Command: cmd, Output: msg, ExitCode: -1}
		}
		if res.ExitCode != 0 {
			o.log.Errorf("verify failed: %s (exit %d)", cmd, res.ExitCode)
			o.events.Record("verify", node.ID, map[string]any{
				"command": cmd, "ok": false, "exit_code": res.ExitCode,
				"duration_ms": duration,
				"output_tail": tailClip(combined.String(), evVerifyTail),
			})
			return VerifyResult{OK: false, Command: cmd, Output: combined.String(), ExitCode: res.ExitCode}
		}
		o.log.Okf("verify passed: %s", cmd)
		o.events.Record("verify", node.ID, map[string]any{
			"command": cmd, "ok": true, "exit_code": 0,
			"duration_ms": duration,
		})
	}
	// expected_output is a DoD-level assertion: it only has to appear in the
	// combined output of all commands (a quiet build followed by a verbose
	// test must still pass), not in every single command's output.
	if node.DoD.ExpectedOutput != "" {
		want := strings.TrimSpace(node.DoD.ExpectedOutput)
		blob := combined.String()
		if !strings.Contains(blob, want) {
			// Weak models often misuse expected_output as a prose description
			// of what should happen ("Build succeeds") rather than text the
			// command prints. When every command succeeded and the expected
			// text is clearly such a phrase (multi-word sentence, not a
			// literal token/string), treat exit codes as the verdict instead
			// of failing on an unmatchable sentence.
			if looksLikeExpectationPhrase(want, blob) {
				o.log.Warnf("expected_output %q reads like a description, not printed text; trusting exit codes", want)
				o.events.Record("verify", node.ID, map[string]any{
					"ok": true, "expected_output": clip(want, 300),
					"note": "phrase-like expectation, trusted exit codes",
				})
				return VerifyResult{OK: true, Output: combined.String()}
			}
			msg := blob + fmt.Sprintf("expected output %q not found\n", want)
			o.log.Errorf("verify output mismatch: expected %q", want)
			o.events.Record("verify", node.ID, map[string]any{
				"ok": false, "command": cmds[len(cmds)-1],
				"expected_output": clip(want, 300),
				"output_tail":     tailClip(blob, evVerifyTail),
			})
			return VerifyResult{OK: false, Command: cmds[len(cmds)-1], Output: msg}
		}
	}
	return VerifyResult{OK: true, Output: combined.String()}
}

// looksLikeExpectationPhrase reports whether an unmatched expected_output is
// a prose description rather than literal output text. Heuristic: it contains
// spaces forming multiple words or common verbs, and the successful commands
// printed essentially nothing (a quiet build/test). A concrete token like
// "ok", "PASS", a number, or a path must still be matched.
func looksLikeExpectationPhrase(want, blob string) bool {
	w := strings.TrimSpace(want)
	words := strings.Fields(w)
	if len(words) < 2 {
		// Single token is almost certainly literal output, keep enforcing it.
		return false
	}
	// Multi-word sentence that starts with a capital / reads like English or
	// Chinese description: treat as prose.
	hasVerb := strings.Contains(strings.ToLower(w), "succeed") ||
		strings.Contains(strings.ToLower(w), "success") ||
		strings.Contains(strings.ToLower(w), "build") ||
		strings.Contains(strings.ToLower(w), "pass") ||
		strings.Contains(strings.ToLower(w), "works") ||
		strings.Contains(strings.ToLower(w), "returns") ||
		strings.Contains(strings.ToLower(w), "应") ||
		strings.Contains(w, "成功") ||
		strings.Contains(w, "应该")
	if !hasVerb {
		return false
	}
	// Only relax when the commands genuinely produced no stdout: if they
	// printed something, a real mismatch should still fail.
	return commandOutputEmpty(blob)
}

// commandOutputEmpty checks whether the captured combined output carries no
// command stdout/stderr body (only the "$ cmd / exit 0" framing).
func commandOutputEmpty(blob string) bool {
	for _, line := range strings.Split(blob, "\n") {
		if strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "exit ") {
			continue
		}
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}
