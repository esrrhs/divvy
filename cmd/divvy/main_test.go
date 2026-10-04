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

// TestRun_RejectsNegativeBudgetFlags covers the guardrails that stop a
// nonsensical budget from silently disabling a limit the operator intended.
func TestRun_RejectsNegativeBudgetFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"negative max-elapsed", []string{"-max-elapsed", "-5s"}, "-max-elapsed must be >= 0"},
		{"negative max-retries", []string{"-max-retries", "-1"}, "-max-retries must be >= 0"},
		{"negative max-cost", []string{"-max-cost", "-1"}, "-max-cost must be >= 0"},
		{"negative budget-tokens", []string{"-budget-tokens", "-1"}, "-budget-tokens must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"-workdir", t.TempDir(),
				"-datadir", t.TempDir(),
				"-api-key", "dummy",
			}, tc.args...)
			args = append(args, "goal")
			err := run(args)
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestRun_RequiresGoalOrResume keeps the two entry paths distinct.
func TestRun_RequiresGoalOrResume(t *testing.T) {
	err := run([]string{
		"-workdir", t.TempDir(),
		"-datadir", t.TempDir(),
		"-api-key", "dummy",
	})
	if err == nil {
		t.Fatal("expected an error when neither a goal nor -resume is given")
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
