package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewNoisePath(t *testing.T) {
	for _, p := range []string{
		".divvy/solo.jsonl",
		"build/CMakeCache.txt",
		"target/debug/incremental/foo.lock",
		"node_modules/x.js",
		"a.o",
	} {
		if !reviewNoisePath(p) {
			t.Errorf("%q must be noise", p)
		}
	}
	if reviewNoisePath("src/main.cpp") {
		t.Error("source must not be noise")
	}
	if reviewNoisePath("src/main.rs") {
		t.Error("Rust source must not be noise")
	}
}

func TestReviewDiff_ExcludesDivvyAndBuild(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	divvy := filepath.Join(sb.Root, ".divvy")
	if err := os.MkdirAll(divvy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(divvy, "solo.jsonl"), []byte("fmt.Println(\"debug\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := filepath.Join(sb.Root, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "junk.go"), []byte("package junk\nfunc init(){ fmt.Println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("main.go", "package main\n\n// ok\n"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.ReviewDiff(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".divvy") || strings.Contains(out, "solo.jsonl") {
		t.Fatalf(".divvy must be excluded from review_diff:\n%s", out)
	}
	gate, err := sb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gate != nil && (strings.Contains(gate.Report, ".divvy") || strings.Contains(gate.Report, "build/")) {
		t.Fatalf("pre-finish must ignore .divvy/build noise:\n%s", gate.Report)
	}
}
