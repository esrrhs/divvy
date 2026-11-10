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
		if i == 1 {
			newB.WriteString("X1\n")
		} else if i == 10 {
			newB.WriteString("X10\n")
		} else {
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
