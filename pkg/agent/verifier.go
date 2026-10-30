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
	cmds = o.retargetCommands(cmds, sb)

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
		// One retry with a longer budget when the default-ish timeout fires:
		// weak-model DoD often leaves TimeoutSec at 60 while go test needs more
		// on a cold cache. Cap the bump so a hung command cannot run forever.
		if res.TimedOut && timeout < 5*time.Minute {
			retryTimeout := timeout * 2
			if retryTimeout > 5*time.Minute {
				retryTimeout = 5 * time.Minute
			}
			if retryTimeout > timeout {
				o.log.Warnf("verify timeout on %s; retrying once with %s", cmd, retryTimeout)
				o.events.Record("verify_timeout_retry", node.ID, map[string]any{
					"command": cmd, "from_ms": timeout.Milliseconds(), "to_ms": retryTimeout.Milliseconds(),
				})
				retryStart := time.Now()
				retryRes, retryErr := sb.RunBash(ctx, cmd, retryTimeout)
				duration += time.Since(retryStart).Milliseconds()
				if retryErr == nil {
					res = retryRes
				}
			}
		}
		fmt.Fprintf(&combined, "$ %s\nexit %d\n%s\n%s\n", cmd, res.ExitCode, res.Stdout, res.Stderr)
		if res.TimedOut {
			msg := o.withDiagnostics(sb, combined.String()+"timed out\n")
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
			if fbCmd, ok := asFixedStringGrep(cmd); ok {
				fbRes, fbErr := sb.RunBash(ctx, fbCmd, timeout)
				if fbErr == nil && fbRes.ExitCode == 0 && !fbRes.TimedOut {
					o.log.Warnf("verify: grep failed as regex, but succeeded with -F (%s); accepting match", fbCmd)
					res = fbRes
					fmt.Fprintf(&combined, "$ %s (fallback -F)\nexit 0\n%s\n%s\n", fbCmd, fbRes.Stdout, fbRes.Stderr)
				}
			}
		}
		if res.ExitCode != 0 {
			o.log.Errorf("verify failed: %s (exit %d)", cmd, res.ExitCode)
			o.events.Record("verify", node.ID, map[string]any{
				"command": cmd, "ok": false, "exit_code": res.ExitCode,
				"duration_ms": duration,
				"output_tail": tailClip(combined.String(), evVerifyTail),
			})
			return VerifyResult{OK: false, Command: cmd, Output: o.withDiagnostics(sb, combined.String()), ExitCode: res.ExitCode}
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

// retargetCommands rewrites absolute workspace paths baked into DoD commands
// so they point at the sandbox the command is actually executed in.
//
// Why: the decomposer only ever sees the shared workspace, so models routinely
// emit DoD commands like "cd /workspace && go test ./...". A leaf then runs in
// an isolated mirror whose sandbox cwd is already correct, but that explicit cd
// overrides it and verification silently inspects the *unmodified* workspace —
// Go answers "matched no packages", the DoD can never pass, the mirror never
// merges, and every retry burns budget on a failure unrelated to the code.
//
// Replacing the root prefix (rather than stripping the cd) keeps subpaths
// meaningful: "cd /workspace/internal" becomes the mirror's internal. It is a
// no-op whenever the sandbox IS the workspace, so non-isolated runs, goal-level
// acceptance and bare unit callers keep their current behaviour.
func (o *Orchestrator) retargetCommands(cmds []string, sb *tools.Sandbox) []string {
	if o.sandbox == nil || sb == nil {
		return cmds
	}
	ws := o.sandbox.Root
	if ws == "" || ws == sb.Root {
		return cmds
	}
	out := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		out = append(out, strings.ReplaceAll(cmd, ws, sb.Root))
	}
	return out
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

// withDiagnostics prepends a compact compiler/test error summary (Go
// workspaces only) to the raw verification output. Retried workers receive
// prevError head-truncated in their prompt, so the root causes must lead;
// without this the 24KB-capped raw log can bury the one line that matters.
func (o *Orchestrator) withDiagnostics(sb *tools.Sandbox, raw string) string {
	if compact := tools.CompactDiagnostics(tools.DetectProject(sb.Root), raw); compact != "" {
		return compact + "\n" + raw
	}
	return raw
}

// asFixedStringGrep checks if cmd invokes grep without fixed-string or extended-regex flags,
// and returns the command rewritten with `grep -F ` so literal code strings (like Go function signatures) match.
func asFixedStringGrep(cmd string) (string, bool) {
	trimmed := strings.TrimSpace(cmd)
	if !strings.HasPrefix(trimmed, "grep ") && !strings.Contains(trimmed, " grep ") && !strings.Contains(trimmed, "|grep ") {
		return "", false
	}
	fields := strings.Fields(cmd)
	isGrep := false
	for _, f := range fields {
		if f == "grep" {
			isGrep = true
			continue
		}
		if isGrep && strings.HasPrefix(f, "-") {
			if strings.ContainsAny(f, "FEPE") {
				return "", false
			}
		}
		if isGrep && !strings.HasPrefix(f, "-") {
			break
		}
	}
	idx := strings.Index(cmd, "grep ")
	if idx < 0 {
		return "", false
	}
	return cmd[:idx] + "grep -F " + cmd[idx+5:], true
}
