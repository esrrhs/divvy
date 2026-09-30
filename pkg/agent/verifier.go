package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/models"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
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
		ensureDoD(&node.DoD, node.Contract.Outputs, tools.HasGoMod(sb.Root))
		cmds = node.DoD.Commands
	}
	timeout := time.Duration(node.DoD.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	var combined strings.Builder
	for _, cmd := range cmds {
		o.log.Actionf("verify: %s", cmd)
		res, err := sb.RunBash(ctx, cmd, timeout)
		if err != nil {
			msg := fmt.Sprintf("command %q failed to start: %v", cmd, err)
			o.log.Errorf("%s", msg)
			return VerifyResult{OK: false, Command: cmd, Output: msg, ExitCode: -1}
		}
		fmt.Fprintf(&combined, "$ %s\nexit %d\n%s\n%s\n", cmd, res.ExitCode, res.Stdout, res.Stderr)
		if res.TimedOut {
			msg := combined.String() + "timed out\n"
			o.log.Errorf("verify timeout: %s", cmd)
			return VerifyResult{OK: false, Command: cmd, Output: msg, ExitCode: -1}
		}
		if res.ExitCode != 0 {
			o.log.Errorf("verify failed: %s (exit %d)", cmd, res.ExitCode)
			return VerifyResult{OK: false, Command: cmd, Output: combined.String(), ExitCode: res.ExitCode}
		}
		o.log.Okf("verify passed: %s", cmd)
	}
	// expected_output is a DoD-level assertion: it only has to appear in the
	// combined output of all commands (a quiet build followed by a verbose
	// test must still pass), not in every single command's output.
	if node.DoD.ExpectedOutput != "" {
		blob := combined.String()
		if !strings.Contains(blob, node.DoD.ExpectedOutput) {
			msg := blob + fmt.Sprintf("expected output %q not found\n", node.DoD.ExpectedOutput)
			o.log.Errorf("verify output mismatch: expected %q", node.DoD.ExpectedOutput)
			return VerifyResult{OK: false, Command: cmds[len(cmds)-1], Output: msg}
		}
	}
	return VerifyResult{OK: true, Output: combined.String()}
}
