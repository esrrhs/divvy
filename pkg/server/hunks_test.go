package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/tools"
)

func TestApplyHunkDecisionsPartial(t *testing.T) {
	// Keep the two edits more than 2*radius lines apart so they stay
	// separate hunks (radius is 3).
	var oldB, newB strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&oldB, "L%d\n", i)
		switch i {
		case 1:
			newB.WriteString("X1\n")
		case 10:
			newB.WriteString("X10\n")
		default:
			fmt.Fprintf(&newB, "L%d\n", i)
		}
	}
	old, new := oldB.String(), newB.String()
	_, hunks, err := applyHunkDecisions(old, new, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 2 {
		t.Fatalf("hunks=%d want 2", len(hunks))
	}
	// Reject the second hunk only (X10 stays L10).
	got, _, err := applyHunkDecisions(old, new, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "X1\n") || !strings.Contains(got, "L10\n") || strings.Contains(got, "X10\n") {
		t.Fatalf("partial=%q", got)
	}
	allRej, _, err := applyHunkDecisions(old, new, []bool{false, false})
	if err != nil {
		t.Fatal(err)
	}
	if allRej != old {
		t.Fatalf("reject all=%q want old", allRej)
	}
	allAcc, _, err := applyHunkDecisions(old, new, []bool{true, true})
	if err != nil {
		t.Fatal(err)
	}
	if allAcc != new {
		t.Fatalf("accept all=%q want new", allAcc)
	}
}

func TestApplyHunkDecisionsAddedAndDeleted(t *testing.T) {
	added, hunks, err := applyHunkDecisions("", "hi\n", []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 {
		t.Fatalf("added hunks=%d", len(hunks))
	}
	if added != "" {
		t.Fatalf("reject add should be empty, got %q", added)
	}
	kept, _, err := applyHunkDecisions("", "hi\n", []bool{true})
	if err != nil {
		t.Fatal(err)
	}
	if kept != "hi\n" {
		t.Fatalf("accept add=%q", kept)
	}
	deleted, _, err := applyHunkDecisions("old\n", "", []bool{true})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != "" {
		t.Fatalf("accept delete=%q", deleted)
	}
	restored, _, err := applyHunkDecisions("old\n", "", []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if restored != "old\n" {
		t.Fatalf("reject delete=%q", restored)
	}
}

func TestParseHunkDecisions(t *testing.T) {
	got, err := parseHunkDecisions(3, []hunkDecision{{Index: 1, Accept: false}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0] || got[1] || !got[2] {
		t.Fatalf("%v", got)
	}
	if _, err := parseHunkDecisions(1, []hunkDecision{{Index: 4, Accept: true}}, ""); err == nil {
		t.Fatal("expected range error")
	}
	all, err := parseHunkDecisions(2, nil, "reject")
	if err != nil || all[0] || all[1] {
		t.Fatalf("reject all: %v %v", all, err)
	}
}

func TestApplyHunkDecisionsNoChange(t *testing.T) {
	s := "same\n"
	got, hunks, err := applyHunkDecisions(s, s, nil)
	if err != nil || got != s || len(hunks) != 0 {
		t.Fatalf("got=%q hunks=%d err=%v", got, len(hunks), err)
	}
}

func TestKeepTrailingNewline(t *testing.T) {
	if !strings.HasSuffix(keepTrailingNL("a\n", "b\n", "b"), "\n") {
		t.Fatal("should restore trailing newline")
	}
}

func TestPatchesFromApprovalHunksPartial(t *testing.T) {
	// Keep the two edits more than 2*radius lines apart so they stay two hunks.
	var old, neu strings.Builder
	old.WriteString("keep\n")
	for i := 0; i < 10; i++ {
		old.WriteString("pad\n")
	}
	old.WriteString("alpha\n")
	neu.WriteString("keep\n")
	for i := 0; i < 10; i++ {
		neu.WriteString("pad\n")
	}
	neu.WriteString("beta\n")
	changes := []tools.FileChange{{
		Path:       "x.txt",
		Status:     tools.ChangeModified,
		OldContent: old.String(),
		NewContent: neu.String(),
	}}
	patches, deletes, err := patchesFromApprovalHunks(changes, map[string]applyHunkRequest{
		"x.txt": {Path: "x.txt", All: "reject"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 {
		t.Fatalf("deletes=%v", deletes)
	}
	if patches["x.txt"] != old.String() {
		t.Fatalf("reject-all should restore old, got %q", patches["x.txt"])
	}
}

func TestViewApprovalChangesIncludesHunks(t *testing.T) {
	views := viewApprovalChanges([]tools.FileChange{{
		Path:       "a.txt",
		Status:     tools.ChangeAdded,
		NewContent: "hi\n",
	}})
	if len(views) != 1 {
		t.Fatalf("len=%d", len(views))
	}
	if views[0]["hunk_count"] != 1 {
		t.Fatalf("expected 1 hunk, got %#v", views[0]["hunk_count"])
	}
	if _, ok := views[0]["hunks"]; !ok {
		t.Fatal("missing hunks")
	}
}

// TestApplyGroupDecisions: two changes a few lines apart land in ONE hunk
// (the context around them overlaps), so per-hunk decisions cannot keep one
// and drop the other.
func TestApplyGroupDecisions(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	// Two changes, five lines apart: same hunk, two groups.
	updated := "a\nB\nc\nd\ne\nf\ng\nH\n"
	lines, _ := computeDiffLines(old, updated)
	groups := changeGroups(lines)
	if len(groups) != 2 {
		t.Fatalf("expected 2 change groups, got %d: %+v", len(groups), lines)
	}

	// Keep the first change, drop the second.
	got, _, err := applyGroupDecisions(old, updated, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nB\nc\nd\ne\nf\ng\nh\n"
	if got != want {
		t.Fatalf("kept the first change only: got %q want %q", got, want)
	}

	// Drop the first, keep the second.
	got, _, err = applyGroupDecisions(old, updated, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	want = "a\nb\nc\nd\ne\nf\ng\nH\n"
	if got != want {
		t.Fatalf("kept the second change only: got %q want %q", got, want)
	}

	// A modification is ONE group: rejecting it keeps the old line.
	old2 := "x\ny\n"
	new2 := "x\nY\n"
	got, _, err = applyGroupDecisions(old2, new2, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if got != old2 {
		t.Fatalf("a rejected modification must keep the original line: got %q want %q", got, old2)
	}

	// A mismatched count is a client bug, not something to guess about.
	if _, _, err := applyGroupDecisions(old, updated, []bool{true}); err == nil {
		t.Fatal("a wrong number of decisions must be refused")
	}
}

// The status the write-back reports must not claim "accepted" for a hunk
// whose second change was dropped.
func TestHunkDecisionsFromGroups(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	updated := "a\nB\nc\nd\ne\nf\ng\nH\n"
	lines, _ := computeDiffLines(old, updated)
	hunks := toDiffHunks(lines, hunkContextRadius)

	// Both changes accepted: every hunk is accepted.
	dec := hunkDecisionsFromGroups(lines, hunks, []bool{true, true})
	for i, d := range dec {
		if !d {
			t.Fatalf("hunk %d should be accepted: %v", i, dec)
		}
	}

	// The second change dropped: the hunk it lives in is not accepted.
	dec = hunkDecisionsFromGroups(lines, hunks, []bool{true, false})
	allAccepted := true
	for _, d := range dec {
		if !d {
			allAccepted = false
		}
	}
	if allAccepted {
		t.Fatalf("a dropped change must not be reported as accepted: %v", dec)
	}
}

// Two modifications side by side are TWO groups: the user keeps one and drops
// the other. Grouping the whole run made them inseparable, which is the one
// case a line-level review exists for.
func TestApplyGroupDecisions_SplitsAdjacentModifications(t *testing.T) {
	old := "a\nb\nc\n"
	updated := "A\nB\nc\n"

	// Drop the first modification, keep the second.
	got, _, err := applyGroupDecisions(old, updated, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nB\nc\n"
	if got != want {
		t.Fatalf("keeping only the second modification: got %q want %q", got, want)
	}

	// Drop the second, keep the first.
	got, _, err = applyGroupDecisions(old, updated, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	want = "A\nb\nc\n"
	if got != want {
		t.Fatalf("keeping only the first modification: got %q want %q", got, want)
	}
}
