package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func auditTC(id, name string, args map[string]any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Type: "function", Function: llm.ToolFunction{Name: name, Arguments: string(b)}}
}

func TestAuditManualEscape(t *testing.T) {
	work, data, outside := t.TempDir(), t.TempDir(), t.TempDir()
	dest := filepath.Join(outside, "marker")
	cfg := directCfg(t, work, data, "verify")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	calls := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return jsonAct(tools.ToolRunBash, map[string]any{"command": fmt.Sprintf("printf escaped > %q", dest)}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	reviews := 0
	o.SetLeafApprovalHook(func(context.Context, LeafApprovalRequest) (LeafApprovalDecision, error) {
		reviews++
		return LeafApprovalDecision{Approved: false, Comment: "no"}, nil
	})
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		b, _ := os.ReadFile(dest)
		t.Fatalf("manual/isolate run_bash wrote outside the sandbox: %q", b)
	}
}

func TestAuditSummaryLeaksTree(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "finish")
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "key=" + cfg.APIKey}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(o.storage.GetTreeFilePath(o.SessionID()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), cfg.APIKey) {
		t.Fatalf("synthetic API key persisted in the task tree")
	}
	if strings.Contains(ctl.summary, cfg.APIKey) {
		t.Fatalf("SetSummary received the raw synthetic API key")
	}
}

func TestAuditNativeArgsPersistSecret(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "write")
	cfg.NativeTools = true
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{auditTC("c1", tools.ToolWriteFile, map[string]any{"path": "x", "content": cfg.APIKey})}}, nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(o.storage.GetDirectHistoryPath(o.SessionID()))
	if strings.Contains(string(b), cfg.APIKey) {
		t.Fatalf("native Function.Arguments retained the synthetic API key in the transcript")
	}
}

func TestAuditInFlightPersistsSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	sec := "synthetic-audit-key-no-real-secret"
	if err := saveDirectState(p, &directState{InFlight: &directInFlight{Name: tools.ToolRunBash, Args: map[string]any{"command": "echo " + sec}}}, sec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), sec) {
		t.Fatalf("InFlight.Args stored the synthetic key unredacted")
	}
}

func TestAuditAskNoStillRunsBatch(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "ask")
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.askHook = func(string) string { return "No. Do not proceed." }
	d := NewDirect(o, nil)
	d.st = &directState{}
	_, _, err = d.applyActions(context.Background(), o.tree.RootID, []taggedAction{
		{Name: "ask", Args: map[string]any{"question": "May I delete the file?"}},
		{Name: tools.ToolWriteFile, Args: map[string]any{"path": "marker", "content": "executed despite no"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, "marker")); !os.IsNotExist(err) {
		t.Fatal("batch tool following ask ran after the user answered No")
	}
}

func TestAuditNativeAskMissingResult(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "ask")
	cfg.NativeTools = true
	n := 0
	missing := false
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{auditTC("ask1", "ask", map[string]any{"question": "Which color?"})}}, nil
		}
		has := false
		for _, m := range r.Messages {
			if m.Role == llm.RoleTool && m.ToolCallID == "ask1" {
				has = true
			}
		}
		missing = !has
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.askHook = func(string) string { return "blue" }
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if missing {
		t.Fatal("subsequent native request omitted the ask tool response")
	}
}

func TestAuditBatchPauseDropsRemainder(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "batch")
	cfg.NativeTools = true
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{
				auditTC("a", tools.ToolWriteFile, map[string]any{"path": "a", "content": "a"}),
				auditTC("b", tools.ToolWriteFile, map[string]any{"path": "b", "content": "b"}),
			}}, nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := NewDirect(o, &testDirectCtl{pauseAfter: 1}).Run(context.Background()); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, "b")); err != nil {
		t.Fatalf("after resume the remaining batch action was not completed: %v", err)
	}
}

func TestAuditShutdownDiscardsManualEdits(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "write")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	n := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "staged", "content": "important"}), nil
		}
		if n == 2 {
			cancel()
			return nil, context.Canceled
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := NewDirect(o, nil).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	st, err := loadDirectState(o.storage.GetDirectHistoryPath(o.SessionID()), cfg.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mirror == nil || strings.TrimSpace(st.Mirror.Dir) == "" {
		t.Fatal("cancellation dropped the manual isolate mirror")
	}
	if _, err := os.Stat(filepath.Join(st.Mirror.Dir, "staged")); err != nil {
		t.Fatalf("staged file missing from retained mirror: %v", err)
	}
}

func TestAuditThoughtLeaksNestedEventMessages(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "write")
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			p, _ := json.Marshal(map[string]any{"thought": "key=" + cfg.APIKey, "action": tools.ToolWriteFile, "args": map[string]any{"path": "x", "content": "harmless"}})
			return &llm.Response{Content: string(p)}, nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(o.EventPath())
	if strings.Contains(string(b), cfg.APIKey) {
		t.Fatalf("registered synthetic key leaked into events via nested messages")
	}
}

func TestAuditKnownKeyLeaksSessionLog(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "ask")
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return jsonAct("ask", map[string]any{"question": "Which value for " + cfg.APIKey + "?"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.askHook = func(string) string { return "no" }
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(o.LogPath())
	if strings.Contains(string(b), cfg.APIKey) {
		t.Fatalf("synthetic key in ask was written unmasked to the session log")
	}
}

func TestAuditNativeFinishLeavesUnansweredCall(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "first")
	cfg.NativeTools = true
	n := 0
	unresolved := false
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{auditTC("finish1", tools.ToolFinish, map[string]any{"summary": "done"})}}, nil
		}
		seen, answered := false, false
		for _, m := range r.Messages {
			for _, tc := range m.ToolCalls {
				if tc.ID == "finish1" {
					seen = true
				}
			}
			if m.Role == llm.RoleTool && m.ToolCallID == "finish1" {
				answered = true
			}
		}
		unresolved = seen && !answered
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "second"}), nil
	}}
	o, err := NewFromGoal(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.cfg.Goal = "followup"
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if unresolved {
		t.Fatal("native finish stayed unanswered across a follow-up turn")
	}
}

func TestAuditPauseWhileModelFinishesHangsApproval(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "manual")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	n := 0
	entered, release := make(chan struct{}), make(chan struct{})
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "x", "content": "changed"}), nil
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	m := NewRunManager()
	defer m.Shutdown(time.Second)
	h, err := m.Start(cfg, c, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := h.Pause(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish after Pause during the model call")
	}
	if h.Phase() == PhaseLeafApproval {
		t.Fatal("Pause during a model finish entered manual approval instead of pausing")
	}
	if h.Phase() != PhasePaused {
		t.Fatalf("phase=%s, want paused", h.Phase())
	}
}
