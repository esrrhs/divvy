package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// stallFingerprintLines caps how much raw failure output is folded into a
// fingerprint when the toolchain has no diagnostic rules for it. Only the
// head of the output is used: that is where compilers and test runners put
// the root cause, and it keeps the hash cheap.
const stallFingerprintLines = 12

var stallDigits = regexp.MustCompile(`\d+`)

// stallFingerprint reduces a failure message to a stable signature so two
// attempts that fail the same way hash to the same value.
//
// When the detected toolchain can extract root-cause lines, those lines are
// used directly — they are already de-duplicated, normalized and stripped of
// volatile detail (timings, byte counts), which is exactly what a fingerprint
// needs. Otherwise the head of the raw output is used with digits masked, so
// that "3 failing tests" and "4 failing tests" at the same location still
// compare equal.
//
// An empty message yields an empty fingerprint, which never counts as a stall.
func stallFingerprint(pt tools.ProjectType, msg string) string {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		return ""
	}
	var body string
	if diag := strings.TrimSpace(tools.CompactDiagnostics(pt, trimmed)); diag != "" {
		body = diag
	} else {
		body = headLines(trimmed, stallFingerprintLines)
	}
	body = stallDigits.ReplaceAllString(body, "#")
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// headLines returns the first n non-empty, trimmed lines of s joined by "\n".
func headLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]string, 0, n)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return strings.Join(out, "\n")
}

// stalled reports whether leaf has now failed the same way at least
// cfg.MaxStall times in a row, together with the length of that run.
// Detection is off when MaxStall <= 0.
func (o *Orchestrator) stalled(leafID string) (run int, stalled bool) {
	if o.cfg.MaxStall <= 0 {
		return 0, false
	}
	live, ok := o.tree.CloneNode(leafID)
	if !ok {
		return 0, false
	}
	run = live.StallRun()
	return run, run >= o.cfg.MaxStall
}

// handleStall stops retrying a leaf that keeps failing identically and hands
// it to the normal failure path: re-split into smaller tasks while the
// decomposition budget allows it, otherwise fail the node. Retrying again
// would only repeat an attempt that has already proven it does not converge.
func (o *Orchestrator) handleStall(leaf *models.TaskNode, run int, errMsg string) error {
	msg := fmt.Sprintf("stalled: the same failure repeated %d times (last: %s)", run, truncate(errMsg, 400))
	o.log.Errorf("%s %s", leaf.ID, msg)
	o.events.Record("leaf_stall", leaf.ID, map[string]any{
		"run":   run,
		"limit": o.cfg.MaxStall,
		"error": clip(errMsg, evReasonChars),
	})
	return o.handleLeafFailure(leaf, msg)
}
