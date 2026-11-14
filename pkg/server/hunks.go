package server

import (
	"fmt"
	"strings"
)

const (
	hunkContextRadius = 3
	maxDiffLines      = 4000
)

type diffLine struct {
	Type  string `json:"type"` // context | add | remove
	OldNo *int   `json:"old_no"`
	NewNo *int   `json:"new_no"`
	Text  string `json:"text"`
}

type diffHunk struct {
	Index int        `json:"index"`
	Lines []diffLine `json:"lines"`
	lo    int
	hi    int
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func keepTrailingNL(oldText, newText, result string) string {
	if result == "" {
		return result
	}
	if strings.HasSuffix(oldText, "\n") || strings.HasSuffix(newText, "\n") {
		if !strings.HasSuffix(result, "\n") {
			return result + "\n"
		}
	}
	return result
}

func computeDiffLines(oldText, newText string) ([]diffLine, bool) {
	a := splitDiffLines(oldText)
	b := splitDiffLines(newText)
	truncated := len(a)+len(b) > maxDiffLines
	return lcsWalk(a, b), truncated
}

func lcsWalk(a, b []string) []diffLine {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := 0; i <= m; i++ {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	oldNo, newNo := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			oldNo++
			newNo++
			on, nn := oldNo, newNo
			out = append(out, diffLine{Type: "context", OldNo: &on, NewNo: &nn, Text: a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			oldNo++
			on := oldNo
			out = append(out, diffLine{Type: "remove", OldNo: &on, Text: a[i]})
			i++
		} else {
			newNo++
			nn := newNo
			out = append(out, diffLine{Type: "add", NewNo: &nn, Text: b[j]})
			j++
		}
	}
	for i < m {
		oldNo++
		on := oldNo
		out = append(out, diffLine{Type: "remove", OldNo: &on, Text: a[i]})
		i++
	}
	for j < n {
		newNo++
		nn := newNo
		out = append(out, diffLine{Type: "add", NewNo: &nn, Text: b[j]})
		j++
	}
	return out
}

func toDiffHunks(lines []diffLine, radius int) []diffHunk {
	if radius < 0 {
		radius = hunkContextRadius
	}
	var changeIdx []int
	for i, l := range lines {
		if l.Type != "context" {
			changeIdx = append(changeIdx, i)
		}
	}
	if len(changeIdx) == 0 {
		return nil
	}
	type rng struct{ lo, hi int }
	var ranges []rng
	for _, idx := range changeIdx {
		lo := idx - radius
		if lo < 0 {
			lo = 0
		}
		hi := idx + radius
		if hi > len(lines)-1 {
			hi = len(lines) - 1
		}
		if n := len(ranges); n > 0 && lo <= ranges[n-1].hi+1 {
			if hi > ranges[n-1].hi {
				ranges[n-1].hi = hi
			}
		} else {
			ranges = append(ranges, rng{lo, hi})
		}
	}
	out := make([]diffHunk, 0, len(ranges))
	for i, r := range ranges {
		out = append(out, diffHunk{
			Index: i,
			Lines: append([]diffLine(nil), lines[r.lo:r.hi+1]...),
			lo:    r.lo,
			hi:    r.hi,
		})
	}
	return out
}

func hunkIndexAt(hunks []diffHunk, lineIdx int) int {
	for _, h := range hunks {
		if lineIdx >= h.lo && lineIdx <= h.hi {
			return h.Index
		}
	}
	return -1
}

// applyHunkDecisions rebuilds a file from the original pair of texts.
// decisions[i] is true to keep the proposed (new) side of hunk i, false to
// keep the original (old) side. Missing or extra slots default to accept.
func applyHunkDecisions(oldText, newText string, decisions []bool) (string, []diffHunk, error) {
	lines, _ := computeDiffLines(oldText, newText)
	hunks := toDiffHunks(lines, hunkContextRadius)
	accepted := make([]bool, len(hunks))
	for i := range accepted {
		accepted[i] = true
		if i < len(decisions) {
			accepted[i] = decisions[i]
		}
	}
	if len(hunks) == 0 {
		return keepTrailingNL(oldText, newText, newText), hunks, nil
	}
	var b strings.Builder
	for i, l := range lines {
		hi := hunkIndexAt(hunks, i)
		var keep bool
		switch l.Type {
		case "context":
			keep = true
		case "add":
			keep = hi >= 0 && accepted[hi]
		case "remove":
			keep = hi < 0 || !accepted[hi]
		default:
			return "", nil, fmt.Errorf("unknown diff line type %q", l.Type)
		}
		if !keep {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.Text)
	}
	return keepTrailingNL(oldText, newText, b.String()), hunks, nil
}

// changeGroups returns the groups of changed lines: each maximal run of
// consecutive non-context lines is ONE change.
//
// A modification shows up as a remove line followed by an add line, so it is
// one group, not two — otherwise rejecting "the new version of this line"
// would leave the old line deleted and the file would lose the line entirely.
func changeGroups(lines []diffLine) [][]int {
	var groups [][]int
	for i := 0; i < len(lines); i++ {
		if lines[i].Type == "context" {
			continue
		}
		var removes, adds []int
		for i < len(lines) && lines[i].Type != "context" {
			if lines[i].Type == "remove" {
				removes = append(removes, i)
			} else {
				adds = append(adds, i)
			}
			i++
		}
		// A modification shows up as a remove line plus an add line, so the
		// removes and the adds of a run are PAIRED positionally: the i-th
		// remove belongs to the i-th add. Two modifications that sit next to
		// each other are therefore two groups, and the user can drop one and
		// keep the other. Grouping the whole run instead made them
		// inseparable.
		pairs := len(removes)
		if len(adds) < pairs {
			pairs = len(adds)
		}
		for k := 0; k < pairs; k++ {
			groups = append(groups, []int{removes[k], adds[k]})
		}
		// Whatever is left over is a pure deletion or a pure addition.
		for k := pairs; k < len(removes); k++ {
			groups = append(groups, []int{removes[k]})
		}
		for k := pairs; k < len(adds); k++ {
			groups = append(groups, []int{adds[k]})
		}
	}
	return groups
}

// applyGroupDecisions applies one decision per change group, so the user can
// keep one change and drop the next one even when both land in the same hunk.
//
// Per-hunk decisions are too coarse for that case: two changes a few lines
// apart share a hunk (the context around them overlaps), so rejecting one
// rejects both — and the only way to keep one was to edit the file by hand.
//
// The decisions are indexed by the groups, in the order the UI shows them.
func applyGroupDecisions(oldText, newText string, decisions []bool) (string, []diffHunk, error) {
	lines, _ := computeDiffLines(oldText, newText)
	hunks := toDiffHunks(lines, hunkContextRadius)
	groups := changeGroups(lines)
	if len(decisions) != len(groups) {
		return "", nil, fmt.Errorf("group decisions (%d) do not match the %d changes", len(decisions), len(groups))
	}
	// The output has to follow the FILE's line order, not the diff's.
	//
	// The diff emits a run of changed lines as all the removals first and then
	// all the additions. Walking it line by line therefore puts a KEPT
	// removal (an old line that stays) before the addition that replaces
	// another line — "b" then "A" instead of "A" then "b". The file's lines
	// end up shuffled, which is worse than a rejected change.
	//
	// So the run is emitted group by group: an accepted group contributes its
	// new lines, a rejected one its old lines, and the groups are in position
	// order.
	var b strings.Builder
	write := func(text string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	for i := 0; i < len(lines); {
		l := lines[i]
		switch l.Type {
		case "context":
			write(l.Text)
			i++
			continue
		case "add", "remove":
		default:
			return "", nil, fmt.Errorf("unknown diff line type %q", l.Type)
		}
		// A run of changed lines: emit it group by group.
		runStart := i
		for i < len(lines) && lines[i].Type != "context" {
			i++
		}
		for gi, g := range groups {
			if gi >= len(decisions) || !groupInRange(g, runStart, i) {
				continue
			}
			accepted := decisions[gi]
			for _, li := range g {
				line := lines[li]
				if accepted && line.Type == "add" {
					write(line.Text)
				}
				if !accepted && line.Type == "remove" {
					write(line.Text)
				}
			}
		}
	}
	return keepTrailingNL(oldText, newText, b.String()), hunks, nil
}

// groupInRange reports whether a group's lines all sit inside [start, end).
func groupInRange(g []int, start, end int) bool {
	if len(g) == 0 {
		return false
	}
	for _, i := range g {
		if i < start || i >= end {
			return false
		}
	}
	return true
}

// hunkDecisionsFromGroups folds the per-group decisions back into the
// per-hunk shape the stored status uses: a hunk counts as accepted only when
// all of its groups are. Reporting "accepted" for a hunk whose second change
// was dropped is exactly the kind of lie this project does not tell.
func hunkDecisionsFromGroups(lines []diffLine, hunks []diffHunk, decisions []bool) []bool {
	groups := changeGroups(lines)
	out := make([]bool, len(hunks))
	for i := range out {
		out[i] = true
	}
	for gi, g := range groups {
		if gi >= len(decisions) || decisions[gi] {
			continue
		}
		for _, li := range g {
			if hi := hunkIndexAt(hunks, li); hi >= 0 {
				out[hi] = false
			}
		}
	}
	return out
}

func parseHunkDecisions(n int, items []hunkDecision, all string) ([]bool, error) {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	switch strings.ToLower(strings.TrimSpace(all)) {
	case "accept", "accepted":
		return out, nil
	case "reject", "rejected":
		for i := range out {
			out[i] = false
		}
		return out, nil
	case "":
	default:
		return nil, fmt.Errorf("unknown all=%q (use accept or reject)", all)
	}
	for _, it := range items {
		if it.Index < 0 || it.Index >= n {
			return nil, fmt.Errorf("hunk index %d out of range (0..%d)", it.Index, n-1)
		}
		out[it.Index] = it.Accept
	}
	return out, nil
}

type hunkDecision struct {
	Index  int  `json:"index"`
	Accept bool `json:"accept"`
}

func storeDecisions(dec []bool) []string {
	out := make([]string, len(dec))
	for i, ok := range dec {
		if ok {
			out[i] = "accept"
		} else {
			out[i] = "reject"
		}
	}
	return out
}

func decisionSummary(dec []bool) string {
	if len(dec) == 0 {
		return "accepted"
	}
	acc, rej := 0, 0
	for _, ok := range dec {
		if ok {
			acc++
		} else {
			rej++
		}
	}
	switch {
	case rej == 0:
		return "accepted"
	case acc == 0:
		return "rejected"
	default:
		return "partial"
	}
}
