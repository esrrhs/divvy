package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stale-read gate catches what the anchor check cannot: the file changed
// under the model, but the anchor still matches somewhere, so a patch would
// land — quietly — in the wrong place.
func TestReplaceText_RefusesStaleRead(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	// The user (or gofmt) rewrites the file after that read.
	external := strings.Replace(goSource, "sum := 0", "sum := 1", 1)
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(external), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := sb.ReplaceTextDetail("calc.go",
		"return float64(sum) / float64(len(xs)+1)",
		"return float64(sum) / float64(len(xs))", false)
	var stale *StaleReadError
	if !errors.As(err, &stale) {
		t.Fatalf("want *StaleReadError, got %v", err)
	}
	if stale.Path != "calc.go" {
		t.Fatalf("path should be workspace-relative, got %q", stale.Path)
	}
	if stale.Was == "" || stale.Now == "" || stale.Was == stale.Now {
		t.Fatalf("digests must identify two versions, got was=%q now=%q", stale.Was, stale.Now)
	}
	if !strings.Contains(err.Error(), staleReadPrefix) {
		t.Fatalf("refusal must be tagged for the loop and the log: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(sb.Root, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != external {
		t.Fatalf("a refused edit must leave the file byte-identical")
	}
}

// The model's own edits are not staleness: after replace_lines it knows the
// current content, so a follow-up patch of the same file must go through.
func TestReplaceText_OwnEditKeepsGateOpen(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 0", "sum := 0.0", false); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 0.0", "sum := 4", false); err != nil {
		t.Fatalf("second edit must not be refused as stale: %v", err)
	}
	out, err := sb.ReadFile("calc.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sum := 4") {
		t.Fatalf("both edits should be on disk:\n%s", out)
	}
}

// A file the model never observed in this run is allowed. The gate must not
// become a wall it cannot justify: tree-mode leaves and resumed sessions
// patch files whose history is not in this sandbox.
func TestReplaceText_UnobservedFileAllowed(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(goSource), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 0", "sum := 2", false); err != nil {
		t.Fatalf("patching an unobserved file must be allowed: %v", err)
	}
}

// A ranged read still counts as "the model has seen this file": the digest is
// of the whole file, so a change anywhere invalidates it.
func TestReadFileLines_RecordsObservation(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(goSource), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFileLines("calc.go", 1, 3); err != nil {
		t.Fatal(err)
	}
	external := goSource + "\n// appended by someone else\n"
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(external), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 0", "sum := 2", false); err == nil {
		t.Fatal("a change after a ranged read must still be detected")
	}
}

// write_file states the whole result, so it refreshes what the model knows
// and clears a stale record rather than tripping it.
func TestWriteFile_RefreshesObservation(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(goSource, "sum := 0", "sum := 7", 1)
	if err := sb.WriteFile("calc.go", rewritten); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 7", "sum := 8", false); err != nil {
		t.Fatalf("patch after a whole-file write must be allowed: %v", err)
	}
}

// Multi-edit calls are atomic, so a stale anchor must leave the file alone
// rather than apply the edits before it.
func TestReplaceTexts_RefusesStaleAtomically(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	external := strings.Replace(goSource, "package calc", "package calc // touched", 1)
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(external), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := sb.ReplaceTextsDetail("calc.go", []TextEdit{
		{Old: "sum := 0", New: "sum := 1"},
		{Old: "return float64(sum) / float64(len(xs)+1)", New: "return 0"},
	}, false)
	if err == nil {
		t.Fatal("want a stale-read refusal")
	}
	got, err := os.ReadFile(filepath.Join(sb.Root, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != external {
		t.Fatalf("no edit may be applied when the batch is refused")
	}
}

// Line-range edits depend on the file just as much as anchors do.
func TestReplaceLines_RefusesStaleRead(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(goSource+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceLines("calc.go", 5, 5, "\tsum := 99"); err == nil {
		t.Fatal("replace_lines must be gated too")
	}
}

// A moved file keeps its record: the model saw that content, and the new path
// is where it now lives.
func TestMovePath_MovesObservation(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	if err := sb.MovePath("calc.go", "internal/calc.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("internal/calc.go", "sum := 0", "sum := 3", false); err != nil {
		t.Fatalf("the moved file is still observed: %v", err)
	}
	if _, ok := sb.ObservedFiles()["calc.go"]; ok {
		t.Fatal("the old path must not keep a record")
	}
}

// Deleting a file forgets it; recreating it with different content must not
// be checked against the deleted version.
func TestDeletePath_ForgetsObservation(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("calc.go"); err != nil {
		t.Fatal(err)
	}
	if err := sb.DeletePath("calc.go", false); err != nil {
		t.Fatal(err)
	}
	if got := sb.ObservedFiles(); len(got) != 0 {
		t.Fatalf("deleted file must not stay observed: %v", got)
	}
}

// Path spelling is not identity: "./calc.go" and "calc.go" are the same file,
// so a read of one must gate an edit of the other.
func TestStaleRead_PathSpellingNormalized(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("./calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("./calc.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.Root, "calc.go"), []byte(goSource+"// x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("calc.go", "sum := 0", "sum := 2", false); err == nil {
		t.Fatal("the two spellings must share one record")
	}
}

// The direct loop persists the map into the transcript; restoring it must not
// hand the caller a live view of the sandbox's own state.
func TestObservedFiles_RoundTripIsACopy(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	snap := sb.ObservedFiles()
	if len(snap) != 1 {
		t.Fatalf("want one entry, got %v", snap)
	}
	for k := range snap {
		delete(snap, k)
	}
	if len(sb.ObservedFiles()) != 1 {
		t.Fatal("mutating the returned map must not touch the sandbox")
	}

	other := newAnchorSandbox(t)
	other.RestoreObserved(map[string]string{"calc.go": "deadbeef"})
	if got := other.ObservedFiles()["calc.go"]; got != "deadbeef" {
		t.Fatalf("restore must seed the gate, got %q", got)
	}
	// A newer observation wins: restoring is a seed, not a rollback.
	other.RestoreObserved(map[string]string{"calc.go": "cafebabe"})
	if got := other.ObservedFiles()["calc.go"]; got != "deadbeef" {
		t.Fatalf("restore must not overwrite a live observation, got %q", got)
	}
}

// A whole-file write can destroy the most work at once: the user's edit is
// gone and nothing said so. The patch paths have been gated on this since the
// beginning; the write path was not.
func TestWriteFile_RefusesWhenTheFileChangedSinceTheRead(t *testing.T) {
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sb.Root, "a.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The model reads the file.
	if _, err := sb.ReadFile("a.go"); err != nil {
		t.Fatal(err)
	}
	// Somebody else edits it.
	if err := os.WriteFile(path, []byte("package p\n\nfunc A() { /* theirs */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = sb.WriteFile("a.go", "package p\n\nfunc A() { /* mine */ }\n")
	if err == nil {
		t.Fatal("the write must be refused: the file changed after the model read it")
	}
	var stale *StaleReadError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want a stale-read refusal", err)
	}
	// The user's edit must survive.
	onDisk, _ := os.ReadFile(path)
	if !strings.Contains(string(onDisk), "theirs") {
		t.Fatalf("the user's edit was destroyed:\n%s", onDisk)
	}
}

// The model may rewrite a file it just wrote: that is not a stale read.
func TestWriteFile_AllowsRewritingAFileItJustWrote(t *testing.T) {
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("a.go", "package p\n\nfunc A() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("a.go", "package p\n\nfunc A() { return }\n"); err != nil {
		t.Fatalf("the model's own second write must be allowed: %v", err)
	}
}

// A file nobody read is still allowed to be written (leaf and resume cases).
func TestWriteFile_AllowsAFileNobodyRead(t *testing.T) {
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.Root, "b.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("b.go", "package p\n\nfunc B() {}\n"); err != nil {
		t.Fatalf("writing a file nobody read must be allowed: %v", err)
	}
}
