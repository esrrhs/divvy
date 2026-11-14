package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// The stale-read gate only earns its keep across a turn boundary: within one
// turn the model is usually the only writer. Here it reads a file, the user
// edits that file while the agent is idle, and the follow-up turn patches it
// with an anchor taken from the old copy. The anchor still matches — which is
// exactly how such an edit lands in the wrong place — so the engine has to
// refuse it and demand a re-read.
type staleDriver struct {
	mu       sync.Mutex
	turn     int
	read     bool
	patched  bool
	reRead   bool
	retried  bool
	sawStale bool
	finished string
}

func (d *staleDriver) client() llm.Client {
	return &llm.ScriptedClient{Handle: d.handle}
}

func (d *staleDriver) handle(ctx context.Context, req llm.Request) (*llm.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	last := lastToolResult(req)
	if d.turn == 1 {
		if !d.read {
			d.read = true
			return jsonAct(tools.ToolReadFile, map[string]any{"path": "calc.go"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{
			"summary": "calc.go returns float64(sum) / float64(len(xs)+1).",
		}), nil
	}
	if strings.Contains(last, "[stale-read]") {
		d.sawStale = true
	}
	if !d.patched {
		d.patched = true
		// Anchor copied from the previous turn's read. The user appended a
		// line after that read, but this text still occurs exactly once.
		return jsonAct(tools.ToolReplaceLines, map[string]any{
			"path":       "calc.go",
			"old_string": "sum := 0",
			"new_string": "sum := 2",
		}), nil
	}
	if d.sawStale && !d.reRead {
		d.reRead = true
		return jsonAct(tools.ToolReadFile, map[string]any{"path": "calc.go"}), nil
	}
	if d.reRead && !d.retried {
		d.retried = true
		return jsonAct(tools.ToolReplaceLines, map[string]any{
			"path":       "calc.go",
			"old_string": "sum := 0",
			"new_string": "sum := 3",
		}), nil
	}
	d.finished = last
	return jsonAct(tools.ToolFinish, map[string]any{
		"summary": "re-read calc.go after the stale-read refusal and applied the edit.",
	}), nil
}

func TestDirect_StaleReadRefusedAcrossTurns(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	src := filepath.Join(work, "calc.go")
	if err := os.WriteFile(src, []byte(goSourceFixture), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := directCfg(t, work, data, "What does Average divide by?")
	drv := &staleDriver{turn: 1}
	o, err := NewFromGoal(cfg, drv.client(), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	transcript := filepath.Join(data, o.SessionID()+".direct.json")
	raw, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"observed"`) {
		t.Fatalf("the read must be recorded in the transcript:\n%s", raw)
	}

	// The user edits the file while the agent is idle.
	edited := goSourceFixture + "\n// touched by the user\n"
	if err := os.WriteFile(src, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}

	cfg.Goal = "Now change the running sum initial value to 3."
	drv.turn = 2
	o2, err := NewFromGoal(cfg, drv.client(), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o2.Close)
	o2.cfg.TranscriptPath = transcript
	if err := NewDirect(o2, nil).Run(context.Background()); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}

	if !drv.sawStale {
		t.Fatal("the edit anchored on the previous turn's copy must be refused with [stale-read]")
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "sum := 2") {
		t.Fatalf("the refused edit must not reach the file:\n%s", got)
	}
	if !strings.Contains(string(got), "sum := 3") {
		t.Fatalf("the edit after the re-read should have applied:\n%s", got)
	}
	if !strings.Contains(string(got), "touched by the user") {
		t.Fatal("the edit must be applied on top of the user's change, not instead of it")
	}
}

// goSourceFixture is the file both turns work on: tabs, a single anchor.
const goSourceFixture = `package calc

// Average returns the arithmetic mean of xs.
func Average(xs []int) float64 {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(len(xs)+1)
}
`
