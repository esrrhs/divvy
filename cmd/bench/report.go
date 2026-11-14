package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// reportMeta records the conditions a report was produced under, so two
// reports from different models/endpoints are never confused.
type reportMeta struct {
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	Extra   string `json:"extra,omitempty"`
	// Loop names the engine loop the numbers describe. Two reports with
	// different loops are not comparable row-for-row, only aggregate-to-
	// aggregate, so the field is part of the report identity.
	Loop     string    `json:"loop,omitempty"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at"`
}

// benchReport is the on-disk JSON schema: stable fields only, so reports
// from different runs can be diffed mechanically.
type benchReport struct {
	Meta      reportMeta  `json:"meta"`
	Results   []RunResult `json:"results"`
	Aggregate Aggregate   `json:"aggregate"`
}

// renderTable prints the human-readable per-run table. Columns are fixed
// width so runs line up; the error column takes the remainder.
//
// OK is the harness's verdict: the corpus's own acceptance command, the same
// command for both loops. ENG is the engine's own verdict — for a tree run
// that is the model's goal-level DoD passing. A row with OK=no and ENG=yes is
// the interesting one: the engine accepted a deliverable that does not work.
func renderTable(rs []RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-6s %3s %3s %3s %8s %6s %9s %6s %5s %5s %5s %5s %5s  %s\n",
		"GOAL", "LOOP", "REP", "OK", "ENG", "WALL", "CALLS", "TOKENS", "LEAVES", "ACPT", "VFAIL", "RETRY", "STALL", "RESPLITS", "ERROR")
	for _, r := range rs {
		ok := "no"
		if r.OK {
			ok = "yes"
		}
		eng := "no"
		if r.EngineOK {
			eng = "yes"
		}
		loop := r.Loop
		if loop == "" {
			loop = loopTree
		}
		acpt := "-"
		switch {
		case !r.AcceptRan:
			acpt = "skip"
		case r.AcceptOK:
			acpt = "pass"
		default:
			acpt = "FAIL"
		}
		fmt.Fprintf(&b, "%-14s %-6s %3d %3s %3s %8s %6d %9d %6s %5s %5d %5d %5d %5d  %s\n",
			r.Goal, loop, r.Repeat, ok, eng, r.Wall().String(), r.LLMCalls, r.TotalTokens,
			fmt.Sprintf("%d/%d", r.LeavesDone, r.Leaves), acpt,
			r.VerifyFail, r.Retries, r.Stalls, r.Resplits, r.Error)
	}
	return b.String()
}

// Wall renders the run duration compactly (3.2s, 1m05s, 2m).
func (r RunResult) Wall() time.Duration { return time.Duration(r.WallMS) * time.Millisecond }

// renderAggregate prints the bench-level verdict line.
func renderAggregate(a Aggregate) string {
	return fmt.Sprintf(
		"runs=%d ok=%d (%.0f%%) wall=%s calls=%d tokens=%d cost=$%.4f verify-fail=%d retries=%d stalls=%d resplits=%d self-pass=%d accept-only-pass=%d\n",
		a.Runs, a.OK, a.SuccessRate*100, (time.Duration(a.WallMS) * time.Millisecond).String(),
		a.LLMCalls, a.TotalTokens, a.CostUSD, a.VerifyFail, a.Retries, a.Stalls, a.Resplits,
		a.SelfPass, a.AcceptOnlyPass)
}

// writeReport persists the JSON report. A write failure is reported but does
// not fail the bench: the console output is the primary artifact.
func writeReport(path string, rep benchReport) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return nil
}
