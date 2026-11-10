package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func TestRunManager_BeforeTerminalRunsBeforeDoneAndKeepsWorkdir(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	marker := filepath.Join(data, "turn-record")
	cfg := mgrCfg(work, data, "sess_finalize_"+t.Name())
	cfg.AgentLoop = AgentLoopDirect
	cfg.DirectSteps = 8
	cfg.APIKey = "sk-finalize-test"
	cfg.HoldServe = true
	cfg.Goal = "finish quickly"

	var step int
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if step == 0 {
			step++
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}

	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second); AfterTerminalPublished = nil })

	var order []string
	var duringFinalizeBusy, afterPublishedFree atomic.Bool
	AfterTerminalPublished = func(h *RunHandle) {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("turn record must already exist when Done becomes visible: %v", err)
		}
		if h.Phase() != PhaseDone {
			t.Errorf("published phase=%s, want done", h.Phase())
		}
		unlock, err := mgr.BeginWorkdirWrite(h.Workdir(), "after-done")
		if err != nil {
			t.Errorf("workdir should be free after the terminal phase is published: %v", err)
		} else {
			afterPublishedFree.Store(true)
			unlock()
		}
		order = append(order, "published")
	}

	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	h.SetBeforeTerminal(func(phase RunPhase) {
		if phase != PhaseDone {
			t.Errorf("before terminal phase=%s", phase)
		}
		if _, err := mgr.BeginWorkdirWrite(h.Workdir(), "during-finalize"); err == nil {
			t.Error("workdir must stay reserved while the turn record is written")
		} else {
			duringFinalizeBusy.Store(true)
		}
		if err := os.WriteFile(marker, []byte("recorded"), 0644); err != nil {
			t.Errorf("write marker: %v", err)
		}
		order = append(order, "finalize")
	})
	h.Launch()
	if err := h.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !duringFinalizeBusy.Load() {
		t.Fatal("expected workdir to stay reserved during finalize")
	}
	if !afterPublishedFree.Load() {
		t.Fatal("expected workdir to be free after Done is published")
	}
	if len(order) < 2 || order[0] != "finalize" || order[1] != "published" {
		t.Fatalf("order=%v, want finalize then published", order)
	}
}
