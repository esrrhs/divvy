package main

import (
	"strings"
	"testing"
)

func TestRun_RejectsInteractiveAndGuided(t *testing.T) {
	err := run([]string{
		"-interactive", "-guided",
		"-workdir", t.TempDir(),
		"-datadir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error when -interactive and -guided are both set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusion error, got %v", err)
	}
}
