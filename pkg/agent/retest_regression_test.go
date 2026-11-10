package agent

// Regression copies of the independent retest (forward safety assertions).
// Source: /workspace/divvy-audit/retest/agent_retest.go — that tree is not modified.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func regRtCall(id, name string, args map[string]any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Type: "function", Function: llm.ToolFunction{Name: name, Arguments: string(b)}}
}
func regRtStrict(ms []llm.Message) error {
	pending := map[string]bool{}
	for _, m := range ms {
		if len(pending) > 0 && m.Role != llm.RoleTool {
			return fmt.Errorf("strict protocol: non-tool message %s before %d calls answered", m.Role, len(pending))
		}
		if m.Role == llm.RoleTool {
			if !pending[m.ToolCallID] {
				return fmt.Errorf("strict protocol: orphan result %s", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" || pending[c.ID] {
				return fmt.Errorf("bad call ID")
			}
			pending[c.ID] = true
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("strict protocol: %d calls missing results", len(pending))
	}
	return nil
}
func TestRegressionRetestNativeNormalPauseFollowupPositive(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "native")
	cfg.NativeTools = true
	n := 0
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		if e := regRtStrict(r.Messages); e != nil {
			return nil, e
		}
		n++
		switch n {
		case 1:
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall("q", "ask", map[string]any{"question": "May I make that change?"}), regRtCall("blocked", tools.ToolWriteFile, map[string]any{"path": "blocked", "content": "bad"})}}, nil
		case 2:
			if _, e := os.Stat(filepath.Join(cfg.WorkDir, "blocked")); !os.IsNotExist(e) {
				return nil, fmt.Errorf("denied batch action ran")
			}
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall("a", tools.ToolWriteFile, map[string]any{"path": "a", "content": "a"}), regRtCall("b", tools.ToolWriteFile, map[string]any{"path": "b", "content": "b"})}}, nil
		default:
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall(fmt.Sprintf("f%d", n), tools.ToolFinish, map[string]any{"summary": "verified"})}}, nil
		}
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	o.askHook = func(string) string { return "No" }
	if e = NewDirect(o, &testDirectCtl{pauseAfter: 1}).Run(context.Background()); !errors.Is(e, ErrPaused) {
		t.Fatalf("want pause: %v", e)
	}
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"a", "b"} {
		v, e := os.ReadFile(filepath.Join(cfg.WorkDir, p))
		if e != nil || string(v) != p {
			t.Fatal("remaining batch action not restored")
		}
	}
	o.cfg.Goal = "follow up"
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Log("strict protocol accepted normal ask-denial, batch pause/resume, native finish and next turn")
}
func TestRegressionRetestManualCancelRestoreApproveRejectPositive(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "manual")
	cfg.Isolate = true
	cfg.LeafApproval = LeafApprovalManual
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		switch n {
		case 1:
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "staged", "content": "important"}), nil
		case 2:
			cancel()
			return nil, context.Canceled
		case 4:
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "rejected", "content": "must not merge"}), nil
		default:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
		}
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	if e = NewDirect(o, nil).Run(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(cfg.WorkDir, "staged")); !os.IsNotExist(e) {
		t.Fatal("unapproved change leaked")
	}
	approvals := 0
	o.SetLeafApprovalHook(func(context.Context, LeafApprovalRequest) (LeafApprovalDecision, error) {
		approvals++
		return LeafApprovalDecision{Approved: approvals == 1, Comment: "reject second"}, nil
	})
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	v, e := os.ReadFile(filepath.Join(cfg.WorkDir, "staged"))
	if e != nil || string(v) != "important" {
		t.Fatal("canceled mirror was not recovered and merged")
	}
	o.cfg.Goal = "second"
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(cfg.WorkDir, "rejected")); !os.IsNotExist(e) {
		t.Fatal("rejected change merged")
	}
	if approvals != 2 {
		t.Fatalf("approval count=%d", approvals)
	}
	t.Log("manual cancellation retained edits, resumed approval merged exactly once, next rejected turn did not merge")
}
func TestRegressionRetestManualSymlinkEscapeShouldBeDenied(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "manual")
	cfg.Isolate = true
	cfg.LeafApproval = LeafApprovalManual
	outside := t.TempDir()
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		switch n {
		case 1:
			return jsonAct(tools.ToolRunBash, map[string]any{"command": fmt.Sprintf("ln -s %q escape && test -L escape", outside)}), nil
		case 2:
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "escape/marker", "content": "escaped"}), nil
		default:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
		}
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	reviews := 0
	o.SetLeafApprovalHook(func(context.Context, LeafApprovalRequest) (LeafApprovalDecision, error) {
		reviews++
		return LeafApprovalDecision{Approved: false, Comment: "no"}, nil
	})
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(outside, "marker")); !os.IsNotExist(e) {
		t.Fatalf("manual escape: bwrap-created symlink + parent-process write_file modified outside; approvals=%d", reviews)
	}
}
func TestRegressionRetestNativeAskPauseResumeStrict(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "ask pause")
	cfg.NativeTools = true
	n := 0
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		if e := regRtStrict(r.Messages); e != nil {
			return nil, e
		}
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall("q", "ask", map[string]any{"question": "Color?"}), regRtCall("rest", tools.ToolWriteFile, map[string]any{"path": "rest", "content": "x"})}}, nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	ctl := &testDirectCtl{}
	o.askHook = func(string) string { ctl.pause = true; return "" }
	if e = NewDirect(o, ctl).Run(context.Background()); !errors.Is(e, ErrPaused) {
		t.Fatal(e)
	}
	o.askHook = func(string) string { return "blue" }
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatalf("ask pause/resume broke strict protocol: %v", e)
	}
}
func TestRegressionRetestNativeInFlightRecoveryStrict(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "recover")
	cfg.NativeTools = true
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		if e := regRtStrict(r.Messages); e != nil {
			return nil, e
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	args := map[string]any{"path": "x", "content": "x"}
	st := &directState{Version: 1, Messages: []llm.Message{{Role: llm.RoleSystem, Content: directMarker}, {Role: llm.RoleUser, Content: "recover"}, {Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{regRtCall("flight", tools.ToolWriteFile, args)}}}, InFlight: &directInFlight{Name: tools.ToolWriteFile, Args: args}}
	if e = saveDirectState(o.storage.GetDirectHistoryPath(o.SessionID()), st, cfg.APIKey); e != nil {
		t.Fatal(e)
	}
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatalf("inflight recovery broke strict native protocol: %v", e)
	}
}
func TestRegressionRetestKnownSecretsAllArtifactsPositive(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "secrets")
	cfg.NativeTools = true
	cfg.Stream = true
	n := 0
	key := cfg.APIKey
	c := &llm.ScriptedClient{StreamChunks: 7, Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		n++
		switch n {
		case 1:
			return &llm.Response{Content: `{"thought":"` + key + `"}`, ToolCalls: []llm.ToolCall{regRtCall("w", tools.ToolWriteFile, map[string]any{"path": "key-fixture", "content": key})}}, nil
		case 2:
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall("q", "ask", map[string]any{"question": "Choose for " + key})}}, nil
		default:
			return &llm.Response{Content: `{"thought":"` + key + `","action":"finish","args":{"summary":"` + key + `"}}`}, nil
		}
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	o.askHook = func(string) string { return key }
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	root, _ := o.Tree().CloneNode(o.Tree().RootID)
	if strings.Contains(root.ResultSummary, key) {
		t.Fatal("summary key")
	}
	e = filepath.Walk(cfg.DataDir, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !i.IsDir() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			if strings.Contains(string(b), key) {
				return fmt.Errorf("synthetic key leaked to %s", filepath.Base(p))
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestRegressionRetestManualHTTPRequiresActionConsent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			hits.Add(1)
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "http manual")
	cfg.Isolate = true
	cfg.LeafApproval = LeafApprovalManual
	cfg.WebEnabled = true
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return jsonAct(tools.ToolHTTPRequest, map[string]any{"method": "POST", "url": srv.URL, "body": "synthetic-local-only"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	web, e := tools.NewWebClient("", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	web.AllowLocal = true
	o.sandbox.Web = web
	reviews := 0
	o.SetLeafApprovalHook(func(context.Context, LeafApprovalRequest) (LeafApprovalDecision, error) {
		reviews++
		return LeafApprovalDecision{Approved: false, Comment: "no"}, nil
	})
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	if hits.Load() != 0 {
		t.Fatalf("manual HTTP POST performed before action consent; local mock hits=%d, merge approvals=%d", hits.Load(), reviews)
	}
}

type regRtCancelCtl struct {
	testDirectCtl
	cancel context.CancelFunc
}

func (c *regRtCancelCtl) SetInTool(v bool) {
	c.testDirectCtl.SetInTool(v)
	if !v {
		c.cancel()
	}
}
func TestRegressionRetestNativeBatchCancellationStrict(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "batch cancel")
	cfg.NativeTools = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	c := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		if e := regRtStrict(r.Messages); e != nil {
			return nil, e
		}
		n++
		if n == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{regRtCall("a", tools.ToolWriteFile, map[string]any{"path": "a", "content": "a"}), regRtCall("b", tools.ToolWriteFile, map[string]any{"path": "b", "content": "b"})}}, nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	if e = NewDirect(o, &regRtCancelCtl{cancel: cancel}).Run(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = NewDirect(o, nil).Run(context.Background()); e != nil {
		t.Fatalf("canceled batch resume broke strict protocol: %v", e)
	}
}

func TestRegressionRetestPendingAskSnapshotMasksKey(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "snapshot")
	n := 0
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		n++
		if n == 1 {
			return jsonAct("ask", map[string]any{"question": "Question containing " + cfg.APIKey}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	m := NewRunManager()
	defer m.Shutdown(time.Second)
	h, e := m.Start(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for h.Phase() != PhaseAsk && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.Phase() != PhaseAsk {
		t.Fatal("ask not reached")
	}
	b, _ := json.Marshal(h.Snapshot())
	if strings.Contains(string(b), cfg.APIKey) {
		t.Fatal("known synthetic key exposed unmasked by RunHandle.Snapshot.PendingAsk")
	}
}
func TestRegressionRetestProviderErrorMasksKeyOnDisk(t *testing.T) {
	cfg := directCfg(t, t.TempDir(), t.TempDir(), "error")
	c := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return nil, fmt.Errorf("synthetic provider error echo: %s", cfg.APIKey)
	}}
	o, e := NewFromGoal(cfg, c, SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	if e = NewDirect(o, nil).Run(context.Background()); e == nil {
		t.Fatal("want controlled provider error")
	}
	var leaked []string
	e = filepath.Walk(cfg.DataDir, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !i.IsDir() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			if strings.Contains(string(b), cfg.APIKey) {
				leaked = append(leaked, filepath.Base(p))
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if len(leaked) > 0 {
		t.Fatalf("known synthetic key in failed-run artifacts: %s", strings.Join(leaked, ", "))
	}
}
