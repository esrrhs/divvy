package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

const (
	multiA = "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	multiB = "package p\n\nfunc B() int {\n\treturn 2\n}\n"
)

func writeMultiFiles(t *testing.T, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "a.go"), []byte(multiA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "b.go"), []byte(multiB), 0o644); err != nil {
		t.Fatal(err)
	}
}

// multiHarness answers each file's prompt with its own replacement code. The
// reply is chosen from the "文件：x.go" line, so the test proves the server
// really makes one call per file instead of asking for a structured list.
func multiHarness(t *testing.T, work string, failB bool) (*httptestServer, *llm.ScriptedClient) {
	t.Helper()
	data := t.TempDir()
	var mu sync.Mutex
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		prompt := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(prompt, "文件：b.go") {
			if failB {
				return nil, fmt.Errorf("upstream 503")
			}
			return &llm.Response{Content: "func B() int {\n\treturn 20\n}\n"}, nil
		}
		return &llm.Response{Content: "func A() int {\n\treturn 10\n}\n"}, nil
	}}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })
	return &httptestServer{ts, data}, client
}

// httptestServer keeps the data dir next to the server so tests can inspect
// the undo stack without a second TempDir.
type httptestServer struct {
	*httptest.Server
	data string
}

func multiTargets() []map[string]any {
	return []map[string]any{
		{"path": "a.go", "selection": map[string]any{"start_line": 3, "end_line": 5}},
		{"path": "b.go", "selection": map[string]any{"start_line": 3, "end_line": 5}},
	}
}

func proposalsFrom(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, ok := got["proposals"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("expected 2 proposals, got %v", got["proposals"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.(map[string]any))
	}
	return out
}

func applyItem(p map[string]any, decisions []bool) map[string]any {
	return map[string]any{
		"path":       p["path"],
		"base_hash":  p["base_hash"],
		"start_line": p["start_line"],
		"end_line":   p["end_line"],
		"old_text":   p["old_text"],
		"new_text":   p["new_text"],
		"decisions":  decisions,
	}
}

// The core contract: one instruction -> one code-only call per file -> a
// hash-guarded proposal per file -> one atomic write -> one undo step.
func TestAiEdit_MultiFileGenerateApplyUndo(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, client := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)

	status, got := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets":     multiTargets(),
		"instruction": "把返回值乘十",
	})
	if status != http.StatusOK {
		t.Fatalf("generate status=%d resp=%v", status, got)
	}
	props := proposalsFrom(t, got)
	if anyString(props[0]["new_text"]) != "func A() int {\n\treturn 10\n}\n" ||
		anyString(props[1]["new_text"]) != "func B() int {\n\treturn 20\n}\n" {
		t.Fatalf("proposals were not generated per file: %v / %v", props[0]["new_text"], props[1]["new_text"])
	}
	if anyString(props[0]["old_text"]) != "func A() int {\n\treturn 1\n}\n" {
		t.Fatalf("old_text must be the disk region: %q", props[0]["old_text"])
	}
	// Older clients read the top-level mirror of the first proposal.
	if anyString(got["path"]) != "a.go" || anyString(got["base_hash"]) != anyString(props[0]["base_hash"]) {
		t.Fatalf("legacy response fields do not mirror proposal 0: %v", got)
	}
	// Each file's prompt must see the OTHER file: "update the callers" needs
	// the caller's code, not just the callee's.
	if len(client.Requests) != 2 {
		t.Fatalf("expected one model call per file, got %d", len(client.Requests))
	}
	for _, req := range client.Requests {
		prompt := req.Messages[len(req.Messages)-1].Content
		if !strings.Contains(prompt, "一起改的文件") {
			t.Fatalf("sibling context missing from prompt:\n%s", prompt)
		}
	}

	status, applied := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"proposals": []map[string]any{
			applyItem(props[0], []bool{true}),
			applyItem(props[1], []bool{true}),
		},
	})
	if status != http.StatusOK {
		t.Fatalf("apply status=%d resp=%v", status, applied)
	}
	paths, _ := applied["paths"].([]any)
	if len(paths) != 2 {
		t.Fatalf("both files should be written, paths=%v", applied["paths"])
	}
	a, _ := os.ReadFile(filepath.Join(work, "a.go"))
	b, _ := os.ReadFile(filepath.Join(work, "b.go"))
	if !strings.Contains(string(a), "return 10") || !strings.Contains(string(b), "return 20") {
		t.Fatalf("write did not land in both files: a=%q b=%q", a, b)
	}

	status, undone := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("undo status=%d resp=%v", status, undone)
	}
	upaths, _ := undone["paths"].([]any)
	if len(upaths) != 2 {
		t.Fatalf("one undo step must restore BOTH files, paths=%v", undone["paths"])
	}
	a, _ = os.ReadFile(filepath.Join(work, "a.go"))
	b, _ = os.ReadFile(filepath.Join(work, "b.go"))
	if string(a) != multiA || string(b) != multiB {
		t.Fatalf("undo left half a rewrite behind: a=%q b=%q", a, b)
	}
}

// A conflict on the SECOND file must not leave the first one written.
func TestAiEdit_MultiFileApplyIsAllOrNothing(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)

	status, got := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets":     multiTargets(),
		"instruction": "改",
	})
	if status != http.StatusOK {
		t.Fatalf("generate: %d %v", status, got)
	}
	props := proposalsFrom(t, got)
	// Someone edits b.go between generation and apply.
	if err := os.WriteFile(filepath.Join(work, "b.go"), []byte(multiB+"// external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, applied := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"proposals": []map[string]any{applyItem(props[0], []bool{true}), applyItem(props[1], []bool{true})},
	})
	if status != http.StatusConflict {
		t.Fatalf("stale second file: status=%d resp=%v", status, applied)
	}
	if anyString(applied["conflict_path"]) != "b.go" {
		t.Fatalf("conflict must name the file: %v", applied)
	}
	a, _ := os.ReadFile(filepath.Join(work, "a.go"))
	if string(a) != multiA {
		t.Fatalf("first file was written despite the conflict: %q", a)
	}
	// A refused apply must not create an undo step either.
	status, _ = apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("a refused apply must not be undoable, got %d", status)
	}
}

// Undo is all-or-nothing too: if one file moved on, none are restored.
func TestAiEdit_MultiFileUndoRefusesWhenOneFileMovedOn(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)
	status, got := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets": multiTargets(), "instruction": "改",
	})
	if status != http.StatusOK {
		t.Fatalf("generate: %d %v", status, got)
	}
	props := proposalsFrom(t, got)
	status, _ = apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"proposals": []map[string]any{applyItem(props[0], []bool{true}), applyItem(props[1], []bool{true})},
	})
	if status != http.StatusOK {
		t.Fatalf("apply: %d", status)
	}
	if err := os.WriteFile(filepath.Join(work, "b.go"), []byte(multiB+"// external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, undone := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusConflict {
		t.Fatalf("undo must refuse: status=%d resp=%v", status, undone)
	}
	a, _ := os.ReadFile(filepath.Join(work, "a.go"))
	if !strings.Contains(string(a), "return 10") {
		t.Fatalf("undo restored a.go while b.go was in conflict: %q", a)
	}
}

// A failing write halfway through the round gets compensated.
func TestAiEdit_MultiFileWriteFailureCompensates(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)
	status, got := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets": multiTargets(), "instruction": "改",
	})
	if status != http.StatusOK {
		t.Fatalf("generate: %d %v", status, got)
	}
	props := proposalsFrom(t, got)
	aiEditWriteHook = func(path string) error {
		if path == "b.go" {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	t.Cleanup(func() { aiEditWriteHook = nil })
	status, applied := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"proposals": []map[string]any{applyItem(props[0], []bool{true}), applyItem(props[1], []bool{true})},
	})
	if status != http.StatusInternalServerError {
		t.Fatalf("write failure must be reported: status=%d resp=%v", status, applied)
	}
	a, _ := os.ReadFile(filepath.Join(work, "a.go"))
	b, _ := os.ReadFile(filepath.Join(work, "b.go"))
	if string(a) != multiA || string(b) != multiB {
		t.Fatalf("compensation left a partial rewrite: a=%q b=%q", a, b)
	}
	if !strings.Contains(anyString(applied["error"]), "恢复原状") {
		t.Fatalf("the error must say the other files were restored: %v", applied)
	}
	status, _ = apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("a compensated write must not be undoable, got %d", status)
	}
}

// One flaky model call must not throw away the other files' work — but it
// also must not silently pretend the failed file was rewritten.
func TestAiEdit_MultiFileGenerationFailureIsReportedAndSkipped(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, true)
	id := newAiEditConv(t, h.Server, work)
	status, got := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets": multiTargets(), "instruction": "改",
	})
	if status != http.StatusOK {
		t.Fatalf("generate: %d %v", status, got)
	}
	props := proposalsFrom(t, got)
	if anyString(props[1]["error"]) == "" {
		t.Fatalf("the failed file must report its error: %v", props[1])
	}
	if anyString(props[1]["new_text"]) != anyString(props[1]["old_text"]) {
		t.Fatalf("a failed proposal must be a no-op, not an empty region: %v", props[1])
	}
	status, applied := apiDo(t, http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"proposals": []map[string]any{applyItem(props[0], []bool{true}), applyItem(props[1], []bool{true})},
	})
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, applied)
	}
	paths, _ := applied["paths"].([]any)
	if len(paths) != 1 || anyString(paths[0]) != "a.go" {
		t.Fatalf("only the successful file should be written: %v", applied["paths"])
	}
	b, _ := os.ReadFile(filepath.Join(work, "b.go"))
	if string(b) != multiB {
		t.Fatalf("the failed file must stay untouched: %q", b)
	}
}

// The UI always asks for SSE. A multi-file round cannot stream one mixed
// delta stream, so the wire carries per-file progress events plus one `done`
// with the whole list — this test is the end-to-end contract for that line.
func TestAiEdit_MultiFileStreamsProgressAndDone(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)
	body, _ := json.Marshal(map[string]any{
		"targets":     multiTargets(),
		"instruction": "改",
		"stream":      true,
	})
	req, err := http.NewRequest(http.MethodPost, apiBase(h.Server)+"/conversations/"+id+"/ai-edit?token=test-token", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q, want SSE", ct)
	}
	var counts []int
	var done aiEditResponse
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			switch event {
			case "progress":
				var p struct {
					Done  int    `json:"done"`
					Total int    `json:"total"`
					Path  string `json:"path"`
				}
				if err := json.Unmarshal([]byte(payload), &p); err != nil {
					t.Fatalf("progress payload: %v", err)
				}
				if p.Total != 2 || p.Path == "" {
					t.Fatalf("progress is not per-file: %s", payload)
				}
				counts = append(counts, p.Done)
			case "delta":
				t.Fatalf("a multi-file round must not emit mixed code deltas: %s", payload)
			case "done":
				if err := json.Unmarshal([]byte(payload), &done); err != nil {
					t.Fatalf("done payload: %v", err)
				}
			}
		}
	}
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("progress events=%v, want 1 then 2", counts)
	}
	if len(done.Proposals) != 2 {
		t.Fatalf("done must carry both proposals: %+v", done)
	}
}

func TestAiEdit_MultiFileTargetsAreValidated(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	h, _ := multiHarness(t, work, false)
	id := newAiEditConv(t, h.Server, work)
	url := apiBase(h.Server) + "/conversations/" + id + "/ai-edit?token=test-token"

	status, _ := apiDo(t, http.MethodPost, url, map[string]any{
		"instruction": "改",
		"targets": []map[string]any{
			{"path": "a.go", "selection": map[string]any{"start_line": 3, "end_line": 5}},
			{"path": "./a.go", "selection": map[string]any{"start_line": 3, "end_line": 5}},
		},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate paths must be refused, got %d", status)
	}

	tooMany := make([]map[string]any, 0, aiEditMaxTargets+1)
	for i := 0; i < aiEditMaxTargets+1; i++ {
		tooMany = append(tooMany, map[string]any{"path": "a.go", "selection": map[string]any{"start_line": 3, "end_line": 5}})
	}
	status, _ = apiDo(t, http.MethodPost, url, map[string]any{"instruction": "改", "targets": tooMany})
	if status != http.StatusBadRequest {
		t.Fatalf("too many targets must be refused, got %d", status)
	}
}

// The latest proposal for THIS file is the one a follow-up revises — even
// when the newest turn overall is about another file.
func TestAiEdit_PendingCodeUsesLatestTurnForThatFile(t *testing.T) {
	history := []aiEditTurn{
		{Instruction: "改两处", Files: []aiEditTurnFile{
			{Path: "a.go", Code: "A1"}, {Path: "b.go", Code: "B1"},
		}},
		{Instruction: "只改 b", Files: []aiEditTurnFile{{Path: "b.go", Code: "B2"}}},
	}
	if code, pending := aiEditPendingCode(history, "a.go", "a.go"); !pending || code != "A1" {
		t.Fatalf("a.go's pending proposal was lost: code=%q pending=%v", code, pending)
	}
	if code, pending := aiEditPendingCode(history, "b.go", "a.go"); !pending || code != "B2" {
		t.Fatalf("b.go should revise its latest proposal: code=%q pending=%v", code, pending)
	}
	history[len(history)-1].Files[0].Applied = true
	if _, pending := aiEditPendingCode(history, "b.go", "a.go"); pending {
		t.Fatal("an applied proposal must not be replayed")
	}
	// Legacy single-file turns (empty path) still work.
	legacy := []aiEditTurn{{Instruction: "改", Code: "old", Path: ""}}
	if code, pending := aiEditPendingCode(legacy, "a.go", "a.go"); !pending || code != "old" {
		t.Fatalf("legacy turn not understood: %q %v", code, pending)
	}
}

func TestAiEdit_MultiFileFollowUpRevisesOwnProposal(t *testing.T) {
	work := t.TempDir()
	writeMultiFiles(t, work)
	var mu sync.Mutex
	var prompts []string
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		prompts = append(prompts, req.Messages[len(req.Messages)-1].Content)
		mu.Unlock()
		return &llm.Response{Content: "func A() int {\n\treturn 10\n}\n"}, nil
	}}
	data := t.TempDir()
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })
	id := newAiEditConv(t, ts, work)

	status, _ := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"targets": multiTargets(), "instruction": "改",
		"history": []map[string]any{{
			"instruction": "上一次",
			"files": []map[string]any{
				{"path": "a.go", "code": "func A() int {\n\treturn 99\n}\n"},
				{"path": "b.go", "code": "func B() int {\n\treturn 98\n}\n"},
			},
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("follow-up status=%d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	foundA := false
	for _, prompt := range prompts {
		if strings.Contains(prompt, "文件：a.go") {
			foundA = true
			if !strings.Contains(prompt, "return 99") {
				t.Fatalf("a.go's previous proposal was not replayed into its prompt:\n%s", prompt)
			}
			if strings.Contains(prompt, "return 98") {
				t.Fatalf("b.go's proposal leaked into a.go's virtual file:\n%s", prompt)
			}
		}
	}
	if !foundA {
		t.Fatal("no prompt was built for a.go")
	}
}
