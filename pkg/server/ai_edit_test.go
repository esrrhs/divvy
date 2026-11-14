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
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

const aiEditSrc = `package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b
}

func Mul(a, b int) int {
	return a * b
}
`

func newAiEditConv(t *testing.T, ts *httptest.Server, work string) string {
	t.Helper()
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir":  work,
		"model":    "scripted",
		"base_url": "http://127.0.0.1:1/v1",
	})
	if status != http.StatusCreated {
		t.Fatalf("create conversation: status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	if id == "" {
		t.Fatalf("create conversation: no id in %v", resp)
	}
	return id
}

// aiEditHarness starts a server whose only model reply is `reply`, and returns
// the server plus its data dir so tests can inspect rollback journals.
func aiEditHarness(t *testing.T, work, reply string) (*httptest.Server, string) {
	t.Helper()
	data := t.TempDir()
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: reply}, nil
		},
		StreamChunks: 4,
	}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })
	return ts, data
}

// TestAiEdit_GenerateDiffFromCodeReply is the core contract: the model returns
// code (not JSON) and the server diffs it into per-hunk decisions.
func TestAiEdit_GenerateDiffFromCodeReply(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "calc.go"), []byte(aiEditSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	reply := "// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	ts, _ := aiEditHarness(t, work, "```go\n"+reply+"```")
	id := newAiEditConv(t, ts, work)

	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"path":        "calc.go",
		"instruction": "修正 Add 的实现",
		"selection":   map[string]any{"start_line": 3, "end_line": 6},
	})
	if status != http.StatusOK {
		t.Fatalf("ai-edit status=%d resp=%v", status, got)
	}
	if anyString(got["old_text"]) != "// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a - b\n}\n" {
		t.Fatalf("old_text not the server-side region: %q", anyString(got["old_text"]))
	}
	if anyString(got["new_text"]) != reply {
		t.Fatalf("code fence not stripped: %q", anyString(got["new_text"]))
	}
	if anyString(got["base_hash"]) == "" {
		t.Fatal("base_hash is required so apply can detect concurrent edits")
	}
	hunks, _ := got["hunks"].([]any)
	if len(hunks) == 0 {
		t.Fatalf("expected at least one hunk, got %v", got["hunks"])
	}
	// Hunk line numbers must be file coordinates (the selection starts at line
	// 3), not offsets inside the selection: the dialog labels them like the
	// editor gutter does.
	foundLine := false
	for _, h := range hunks {
		for _, raw := range h.(map[string]any)["lines"].([]any) {
			l := raw.(map[string]any)
			if anyString(l["text"]) == "\treturn a - b" {
				if l["old_no"].(float64) != 5 {
					t.Fatalf("hunk line number=%v, want file line 5", l["old_no"])
				}
				foundLine = true
			}
		}
	}
	if !foundLine {
		t.Fatalf("removed line missing from hunks: %v", hunks)
	}
	preview := anyString(got["preview"])
	if !strings.Contains(preview, "-	return a - b") || !strings.Contains(preview, "+	return a + b") {
		t.Fatalf("preview should show the change: %q", preview)
	}
}

// TestAiEdit_StreamsDeltasAndFinalEvent proves the dialog can render code as
// it arrives instead of blocking on the whole reply.
func TestAiEdit_StreamsDeltasAndFinalEvent(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "calc.go"), []byte(aiEditSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	reply := "func Add(a, b int) int {\n\treturn a + b\n}\n"
	ts, _ := aiEditHarness(t, work, reply)
	id := newAiEditConv(t, ts, work)

	body, _ := json.Marshal(map[string]any{
		"path":        "calc.go",
		"instruction": "fix",
		"stream":      true,
		"selection":   map[string]any{"start_line": 4, "end_line": 6},
	})
	req, err := http.NewRequest(http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q, want SSE", ct)
	}

	var deltas strings.Builder
	var done aiEditResponse
	var safariErr string
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
			case "delta":
				var d struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal([]byte(payload), &d)
				deltas.WriteString(d.Text)
			case "done":
				if err := json.Unmarshal([]byte(payload), &done); err != nil {
					t.Fatalf("done payload: %v (%s)", err, payload)
				}
			case "error":
				var e struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal([]byte(payload), &e)
				safariErr = e.Error
			}
		}
	}
	if safariErr != "" {
		t.Fatalf("stream error: %s", safariErr)
	}
	if deltas.String() != reply {
		t.Fatalf("deltas=%q want %q", deltas.String(), reply)
	}
	if done.NewText != reply || done.StartLine != 4 {
		t.Fatalf("done payload loses the result: %+v", done)
	}
	if len(done.Hunks) == 0 {
		t.Fatal("done payload should carry hunks")
	}
}

// TestAiEdit_FollowUpTurnRevisesTheProposal is the inline-chat behaviour:
// a second instruction must be answered in the context of the first turn's
// proposal, and the diff the user reviews must still be relative to the file
// on disk — so the apply path (base hash + old text) does not change.
func TestAiEdit_FollowUpTurnRevisesTheProposal(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "calc.go"), []byte(aiEditSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	firstCode := "// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	secondCode := "// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\tif a == 0 && b == 0 {\n\t\treturn 0\n\t}\n\treturn a + b\n}\n"

	var seen []llm.Request
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			seen = append(seen, req)
			if len(seen) == 1 {
				return &llm.Response{Content: firstCode}, nil
			}
			return &llm.Response{Content: secondCode}, nil
		},
	}
	ts := apiServerWithClient(t, work, t.TempDir(), func(agent.Config) llm.Client { return client })
	id := newAiEditConv(t, ts, work)

	// Turn 1.
	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"path":        "calc.go",
		"instruction": "修正 Add 的实现",
		"selection":   map[string]any{"start_line": 3, "end_line": 6},
	})
	if status != http.StatusOK {
		t.Fatalf("turn 1 status=%d resp=%v", status, got)
	}
	original := "// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"

	// Turn 2: same selection, a follow-up instruction, and the previous turn
	// replayed as history.
	status, got = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"path":        "calc.go",
		"instruction": "再补一个零值短路",
		"selection":   map[string]any{"start_line": 3, "end_line": 6},
		"history": []map[string]any{
			{"instruction": "修正 Add 的实现", "code": firstCode},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("turn 2 status=%d resp=%v", status, got)
	}

	// The model must have been told what happened before.
	prompt := ""
	for _, m := range seen[1].Messages {
		prompt += m.Content
	}
	if !strings.Contains(prompt, "修正 Add 的实现") {
		t.Fatal("the follow-up prompt lost the previous instruction")
	}
	if !strings.Contains(prompt, "return a + b") {
		t.Fatal("the follow-up prompt lost the previous proposal")
	}
	// And it must have been shown the proposal as the current selection, not
	// the original code: the marked lines (>>) carry the proposal.
	marked := false
	for _, line := range strings.Split(prompt, "\n") {
		if strings.Contains(line, ">>") && strings.Contains(line, "return a + b") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("the follow-up prompt does not carry the previous proposal as the selection:\n%s", prompt)
	}

	// The diff stays relative to the file on disk: old_text is still the
	// original region, so apply can verify it against the real file.
	if anyString(got["old_text"]) != original {
		t.Fatalf("old_text must stay the on-disk region, got %q", anyString(got["old_text"]))
	}
	if anyString(got["new_text"]) != secondCode {
		t.Fatalf("new_text=%q", anyString(got["new_text"]))
	}
	if anyString(got["base_hash"]) != fileSHA256([]byte(aiEditSrc)) {
		t.Fatal("base_hash must still be the hash of the file on disk")
	}

	// Applying the second turn writes the second proposal over the original.
	status, applied := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":       "calc.go",
		"base_hash":  anyString(got["base_hash"]),
		"start_line": 3,
		"end_line":   6,
		"old_text":   anyString(got["old_text"]),
		"new_text":   anyString(got["new_text"]),
	})
	if status != http.StatusOK {
		t.Fatalf("apply status=%d resp=%v", status, applied)
	}
	onDisk, _ := os.ReadFile(filepath.Join(work, "calc.go"))
	if !strings.Contains(string(onDisk), "if a == 0 && b == 0") {
		t.Fatalf("disk content did not get the second proposal:\n%s", onDisk)
	}
	if strings.Contains(string(onDisk), "return a - b") {
		t.Fatalf("the original line survived the apply:\n%s", onDisk)
	}
}

// A follow-up with no proposal to revise is a client bug, not something to
// guess about.
func TestAiEdit_FollowUpWithoutCodeIsRejected(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "calc.go"), []byte(aiEditSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"path":        "calc.go",
		"instruction": "再改一次",
		"selection":   map[string]any{"start_line": 3, "end_line": 6},
		"history":     []map[string]any{{"instruction": "上一轮", "code": "   "}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d resp=%v, want 400", status, got)
	}
}

// The prompt carries every earlier turn, so an endless conversation would
// grow it without bound. The oldest turns are dropped.
func TestAiEdit_HistoryIsCapped(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "calc.go"), []byte(aiEditSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen []llm.Request
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			seen = append(seen, req)
			return &llm.Response{Content: "func Add(a, b int) int {\n\treturn a + b\n}\n"}, nil
		},
	}
	ts := apiServerWithClient(t, work, t.TempDir(), func(agent.Config) llm.Client { return client })
	id := newAiEditConv(t, ts, work)

	var history []map[string]any
	for i := 1; i <= 10; i++ {
		history = append(history, map[string]any{
			"instruction": fmt.Sprintf("第%d轮", i),
			"code":        "func Add(a, b int) int {\n\treturn a + b\n}\n",
		})
	}
	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit?token=test-token", map[string]any{
		"path":        "calc.go",
		"instruction": "再来一轮",
		"selection":   map[string]any{"start_line": 3, "end_line": 6},
		"history":     history,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d resp=%v", status, got)
	}

	prompt := ""
	for _, m := range seen[0].Messages {
		prompt += m.Content
	}
	if strings.Contains(prompt, "第1轮") {
		t.Fatal("the oldest turn should have been dropped")
	}
	if !strings.Contains(prompt, "第10轮") {
		t.Fatal("the newest turn must be replayed")
	}
}

// TestAiEdit_ApplyHonoursDecisions is the Cursor "accept some hunks" flow:
// rejected hunks must leave the original lines untouched.
func TestAiEdit_ApplyHonoursDecisions(t *testing.T) {
	work := t.TempDir()
	srcLines := []string{
		"package p", "", "func A() int {", "\treturn 1", "}",
		"// 1", "// 2", "// 3", "// 4", "// 5", "// 6", "// 7",
		"func B() int {", "\treturn 2", "}",
	}
	src := strings.Join(srcLines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, data := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	oldRegion := strings.Join(srcLines[2:15], "\n") + "\n"
	newRegion := strings.Replace(oldRegion, "return 1", "return 10", 1)
	newRegion = strings.Replace(newRegion, "return 2", "return 20", 1)
	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":       "p.go",
		"base_hash":  fileSHA256([]byte(src)),
		"start_line": 3,
		"end_line":   15,
		"old_text":   oldRegion,
		"new_text":   newRegion,
		"decisions":  []bool{true, false},
	})
	if status != http.StatusOK {
		t.Fatalf("apply status=%d resp=%v", status, got)
	}
	want := strings.Replace(src, "return 1", "return 10", 1)
	if anyString(got["new_content"]) != want {
		t.Fatalf("partial apply content=%q want %q", anyString(got["new_content"]), want)
	}
	onDisk, _ := os.ReadFile(filepath.Join(work, "p.go"))
	if string(onDisk) != want {
		t.Fatalf("disk content=%q want %q", onDisk, want)
	}
	if got["applied"].(float64) != 1 || got["rejected"].(float64) != 1 {
		t.Fatalf("applied/rejected=%v/%v", got["applied"], got["rejected"])
	}
	// Undo must remain possible: apply records the reverse of what it wrote.
	if _, err := os.Stat(aiEditUndoPath(data, id)); err != nil {
		t.Fatalf("apply should record enough to undo the write: %v", err)
	}
	status, undo := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("undo status=%d resp=%v", status, undo)
	}
	restored, _ := os.ReadFile(filepath.Join(work, "p.go"))
	if string(restored) != src {
		t.Fatalf("undo content=%q want %q", restored, src)
	}
}

// TestAiEdit_UndoReachesBackThroughTheStack: two inline edits in a row must
// leave BOTH undoable. One slot meant the first edit was unreachable — the
// only way back was to hand-edit the file.
func TestAiEdit_UndoReachesBackThroughTheStack(t *testing.T) {
	work := t.TempDir()
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	region := "func A() int {\n\treturn 1\n}\n"
	first := "func A() int {\n\treturn 10\n}\n"
	second := "func A() int {\n\treturn 100\n}\n"

	apply := func(baseHash, oldText, newText string) {
		t.Helper()
		status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
			"path":       "p.go",
			"base_hash":  baseHash,
			"start_line": 3,
			"end_line":   5,
			"old_text":   oldText,
			"new_text":   newText,
		})
		if status != http.StatusOK {
			t.Fatalf("apply status=%d resp=%v", status, got)
		}
	}

	apply(fileSHA256([]byte(src)), region, first)
	afterFirst := "package p\n\n" + first
	apply(fileSHA256([]byte(afterFirst)), first, second)

	undo := func() map[string]any {
		t.Helper()
		status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
		if status != http.StatusOK {
			t.Fatalf("undo status=%d resp=%v", status, got)
		}
		return got
	}

	// First undo: back to the state after the first edit, one undo left.
	got := undo()
	if got["remaining"].(float64) != 1 {
		t.Fatalf("remaining = %v, want 1", got["remaining"])
	}
	onDisk, _ := os.ReadFile(filepath.Join(work, "p.go"))
	if string(onDisk) != afterFirst {
		t.Fatalf("after the first undo: %q want %q", onDisk, afterFirst)
	}

	// Second undo: back to the original, nothing left.
	got = undo()
	if got["remaining"].(float64) != 0 {
		t.Fatalf("remaining = %v, want 0", got["remaining"])
	}
	onDisk, _ = os.ReadFile(filepath.Join(work, "p.go"))
	if string(onDisk) != src {
		t.Fatalf("after the second undo: %q want %q", onDisk, src)
	}

	// A third undo has nothing to undo.
	status, _ := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("a third undo should be 404, got %d", status)
	}
}

// TestAiEdit_ApplyRejectsStaleBaseHash: a file edited between generation and
// apply must not be silently overwritten.
func TestAiEdit_ApplyRejectsStaleBaseHash(t *testing.T) {
	work := t.TempDir()
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	// Someone else edits the file after generation.
	edited := src + "\n// external edit\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":       "p.go",
		"base_hash":  fileSHA256([]byte(src)),
		"start_line": 3,
		"end_line":   5,
		"old_text":   "func A() int {\n\treturn 1\n}\n",
		"new_text":   "func A() int {\n\treturn 2\n}\n",
	})
	if status != http.StatusConflict {
		t.Fatalf("stale base: status=%d resp=%v", status, got)
	}
	onDisk, _ := os.ReadFile(filepath.Join(work, "p.go"))
	if string(onDisk) != edited {
		t.Fatalf("conflict must not write: %q", onDisk)
	}
}

// TestAiEdit_ApplyRejectsRegionMismatch covers a client that sends a stale
// selection range: the range text no longer matches, so nothing is written.
func TestAiEdit_ApplyRejectsRegionMismatch(t *testing.T) {
	work := t.TempDir()
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":       "p.go",
		"base_hash":  fileSHA256([]byte(src)),
		"start_line": 3,
		"end_line":   5,
		"old_text":   "func A() int {\n\treturn 999\n}\n",
		"new_text":   "func A() int {\n\treturn 2\n}\n",
	})
	if status != http.StatusConflict {
		t.Fatalf("region mismatch: status=%d resp=%v", status, got)
	}
}

// TestAiEdit_InsertMode adds new code at a line instead of replacing code.
func TestAiEdit_InsertMode(t *testing.T) {
	work := t.TempDir()
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":        "p.go",
		"base_hash":   fileSHA256([]byte(src)),
		"insert_line": 3,
		"old_text":    "",
		"new_text":    "// B is new.\nfunc B() int {\n\treturn 2\n}",
	})
	if status != http.StatusOK {
		t.Fatalf("insert status=%d resp=%v", status, got)
	}
	want := "package p\n\n// B is new.\nfunc B() int {\n\treturn 2\n}\nfunc A() int {\n\treturn 1\n}\n"
	if anyString(got["new_content"]) != want {
		t.Fatalf("insert content=%q want %q", anyString(got["new_content"]), want)
	}
}

func TestAiEditRegionFor(t *testing.T) {
	content := "a\nb\nc\n"
	r, err := aiEditRegionFor(content, aiEditSelection{StartLine: 2, EndLine: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.OldText != "b\n" {
		t.Fatalf("region=%q", r.OldText)
	}
	if content[:r.StartOffset]+content[r.EndOffset:] != "a\nc\n" {
		t.Fatalf("offsets do not cut the region out: %q", content[:r.StartOffset]+content[r.EndOffset:])
	}
	// No trailing newline at EOF.
	r, err = aiEditRegionFor("a\nb", aiEditSelection{StartLine: 2, EndLine: 2})
	if err != nil || r.OldText != "b" {
		t.Fatalf("eof region=%q err=%v", r.OldText, err)
	}
	if _, err := aiEditRegionFor(content, aiEditSelection{StartLine: 3, EndLine: 9}); err == nil {
		t.Fatal("out-of-range selection must error, not clamp")
	}
	if _, err := aiEditRegionFor(content, aiEditSelection{}); err == nil {
		t.Fatal("empty selection must error, not rewrite the whole file")
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := map[string]string{
		"```go\nfmt.Println(1)\n```": "fmt.Println(1)\n",
		"plain code":                 "plain code",
		"```\na\n```\ntrailing":      "```\na\n```\ntrailing",
	}
	for in, want := range cases {
		if got := stripCodeFence(in); got != want {
			t.Fatalf("stripCodeFence(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAiEdit_UndoRefusesWhenFileMovedOn: undo is a one-slot restore, so it must
// refuse once anything else has written the file.
func TestAiEdit_UndoRefusesWhenFileMovedOn(t *testing.T) {
	work := t.TempDir()
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)

	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/apply?token=test-token", map[string]any{
		"path":       "p.go",
		"base_hash":  fileSHA256([]byte(src)),
		"start_line": 4,
		"end_line":   4,
		"old_text":   "\treturn 1\n",
		"new_text":   "\treturn 2\n",
	})
	if status != http.StatusOK {
		t.Fatalf("apply status=%d resp=%v", status, got)
	}

	// A different actor appends a line after the edit.
	after, _ := os.ReadFile(filepath.Join(work, "p.go"))
	moved := string(after) + "\n// added later\n"
	if err := os.WriteFile(filepath.Join(work, "p.go"), []byte(moved), 0o644); err != nil {
		t.Fatal(err)
	}

	status, got = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusConflict {
		t.Fatalf("undo after external change: status=%d resp=%v", status, got)
	}
	onDisk, _ := os.ReadFile(filepath.Join(work, "p.go"))
	if string(onDisk) != moved {
		t.Fatalf("refused undo must not write: %q", onDisk)
	}
}

// TestAiEdit_UndoWithoutRecord: a fresh conversation has nothing to undo and
// says so instead of touching a file.
func TestAiEdit_UndoWithoutRecord(t *testing.T) {
	work := t.TempDir()
	ts, _ := aiEditHarness(t, work, "unused")
	id := newAiEditConv(t, ts, work)
	status, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/ai-edit/undo?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("undo without record: status=%d resp=%v", status, got)
	}
}
