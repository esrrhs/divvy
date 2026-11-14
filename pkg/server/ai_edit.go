package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Inline edit (Cmd-K) protocol
//
// The model is asked for ONE thing: the replacement code for a line range.
// It is not asked for JSON hunks. Weak models routinely break structured
// output, and a parse failure costs the user a whole round trip; code is what
// they are good at. The server then diffs the returned text against the
// region it sent (computeDiffLines, the same diff the review UI uses) and
// hands the UI per-hunk decisions. That keeps the "accept this line, reject
// that one" affordance without trusting the model to describe its own edit.
//
// Apply is deliberately paranoid: the client must echo the base file hash and
// the exact old region text. If either moved, the whole apply aborts with 409
// and nothing is written, because a silent merge into a file that changed
// under the user is how editors corrupt work.

// aiEditSelection is the line range the user highlighted. Lines are 1-based
// and inclusive. When InsertLine is set the edit is an insertion at that line
// instead of a replacement.
type aiEditSelection struct {
	StartLine  int `json:"start_line,omitempty"`
	EndLine    int `json:"end_line,omitempty"`
	InsertLine int `json:"insert_line,omitempty"`
}

type aiEditRequest struct {
	Path        string          `json:"path"`
	Selection   aiEditSelection `json:"selection"`
	Instruction string          `json:"instruction"`
	// Stream asks for an SSE response with incremental code as it is produced.
	// The final parsed result arrives as the `done` event.
	Stream bool `json:"stream,omitempty"`
}

type aiEditResponse struct {
	Path       string     `json:"path"`
	BaseHash   string     `json:"base_hash"`
	StartLine  int        `json:"start_line"`
	EndLine    int        `json:"end_line"`
	InsertLine int        `json:"insert_line,omitempty"`
	OldText    string     `json:"old_text"`
	NewText    string     `json:"new_text"`
	Hunks      []diffHunk `json:"hunks"`
	Preview    string     `json:"preview"`
	// Truncated reports that the file was too large to send whole and only a
	// window around the selection was used.
	Truncated bool `json:"truncated,omitempty"`
}

type aiEditApplyRequest struct {
	Path     string `json:"path"`
	BaseHash string `json:"base_hash"`
	// StartLine/EndLine locate the region the generation was based on.
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	InsertLine int    `json:"insert_line,omitempty"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	// Decisions is one flag per diff hunk: true keeps the proposed side.
	// Missing or short slices default to accept, matching applyHunkDecisions.
	Decisions []bool `json:"decisions,omitempty"`
}

type aiEditApplyResponse struct {
	Path       string `json:"path"`
	Applied    int    `json:"applied"`
	Rejected   int    `json:"rejected"`
	NewContent string `json:"new_content"`
	Conflict   bool   `json:"conflict"`
}

// aiEditContextChars bounds the file content pasted into the prompt. Beyond
// this the model gets a window around the selection instead of the whole file.
const aiEditContextChars = 24000

// aiEditWindowLines is the context radius used when the file is too large.
const aiEditWindowLines = 80

func (s *Server) handleAiEdit(w http.ResponseWriter, r *http.Request) {
	var req aiEditRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	if strings.TrimSpace(req.Instruction) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instruction is required"})
		return
	}

	convID := r.PathValue("id")
	c, err := s.loadConv(convID)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	_, rel, err := resolveProjectPath(c.WorkDir, path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	sb, err := tools.NewSandbox(c.WorkDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	content, err := sb.ReadFile(rel)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	region, err := aiEditRegionFor(content, req.Selection)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	cfg, err := s.configForConversation(c)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	client := s.newClient(cfg)
	secrets := s.secretsFor(c.ID)

	prompt := buildAiEditPrompt(rel, content, region, req.Instruction)

	// SSE only if the client asked for it and the ResponseWriter can flush.
	flusher, canFlush := w.(http.Flusher)
	stream := req.Stream && canFlush
	if stream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	var streamed strings.Builder
	request := llm.Request{
		Model:       cfg.Model,
		Messages:    prompt,
		Temperature: 0.1,
		MaxTokens:   aiEditMaxTokens(region),
		Stream:      stream,
	}
	if stream {
		request.OnToken = func(delta string) {
			if delta == "" {
				return
			}
			streamed.WriteString(delta)
			// Redact per delta: a split secret can still slip through, so the
			// final payload is redacted again from the accumulated text.
			writeAiEditEvent(w, flusher, "delta", map[string]any{"text": Redact(delta, secrets...)})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), aiEditTimeout(cfg))
	defer cancel()
	resp, err := client.Chat(ctx, request)
	if err != nil {
		if stream {
			writeAiEditEvent(w, flusher, "error", map[string]any{"error": "LLM 调用失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "LLM 调用失败: " + err.Error()})
		return
	}

	raw := resp.Content
	if raw == "" {
		raw = streamed.String()
	}
	newText := stripCodeFence(raw)
	hunks, preview := aiEditDiff(region.OldText, newText, region.StartLine)

	out := aiEditResponse{
		Path:       rel,
		BaseHash:   fileSHA256([]byte(content)),
		StartLine:  region.StartLine,
		EndLine:    region.EndLine,
		InsertLine: region.InsertLine,
		OldText:    Redact(region.OldText, secrets...),
		NewText:    Redact(newText, secrets...),
		Hunks:      redactDiffHunks(hunks, secrets),
		Preview:    Redact(preview, secrets...),
		Truncated:  region.Truncated,
	}
	if stream {
		writeAiEditEvent(w, flusher, "done", out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAiEditApply(w http.ResponseWriter, r *http.Request) {
	var req aiEditApplyRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	if strings.TrimSpace(req.BaseHash) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_hash is required: 需要它判断文件是否在你编辑期间被改动过"})
		return
	}

	convID := r.PathValue("id")
	c, err := s.loadConv(convID)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	_, rel, err := resolveProjectPath(c.WorkDir, path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	unlock, lerr := s.mgr.BeginWorkdirWrite(c.WorkDir, "ai-edit:"+c.ID+":"+rel)
	if lerr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前工作区正被另一任务占用，不能回写：" + lerr.Error()})
		return
	}
	defer unlock()

	// Reload under the lease: the conversation may have been rebound to a
	// different workdir while we waited.
	if latest, e2 := s.loadConv(convID); e2 == nil {
		c = latest
	}

	sb, err := tools.NewSandbox(c.WorkDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	content, err := sb.ReadFile(rel)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if got := fileSHA256([]byte(content)); got != strings.TrimSpace(req.BaseHash) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":         "文件在你生成改动之后被修改过，已停止写回，请重新生成",
			"conflict_path": rel,
		})
		return
	}

	region, err := aiEditRegionFor(content, aiEditSelection{
		StartLine:  req.StartLine,
		EndLine:    req.EndLine,
		InsertLine: req.InsertLine,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if region.OldText != req.OldText {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":         "选中的代码与生成时不一致（文件可能已被外部修改），已停止写回",
			"conflict_path": rel,
		})
		return
	}

	replacement, hunks, err := applyHunkDecisions(region.OldText, req.NewText, req.Decisions)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if region.InsertLine > 0 {
		// An insertion has no old text to inherit line breaks from, so make
		// sure it cannot glue itself onto the neighbouring lines.
		if replacement != "" && !strings.HasSuffix(replacement, "\n") {
			replacement += "\n"
		}
		if region.StartOffset > 0 && content[region.StartOffset-1] != '\n' {
			replacement = "\n" + replacement
		}
	}

	newContent := content[:region.StartOffset] + replacement + content[region.EndOffset:]
	applied, rejected := 0, 0
	for i := range hunks {
		if i < len(req.Decisions) && !req.Decisions[i] {
			rejected++
			continue
		}
		applied++
	}
	if newContent == content {
		writeJSON(w, http.StatusOK, aiEditApplyResponse{
			Path:       rel,
			Applied:    0,
			Rejected:   rejected,
			NewContent: content,
		})
		return
	}

	if err := sb.WriteFile(rel, newContent); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Remember how to undo this write. A later edit overwrites the record:
	// inline edit undo is one level deep, like an editor's Cmd-Z after a
	// single accepted change.
	undo := &aiEditUndoRecord{
		Version:    aiEditUndoVersion,
		ConvID:     c.ID,
		WorkDir:    c.WorkDir,
		Path:       rel,
		Restore:    content,
		BeforeHash: fileSHA256([]byte(content)),
		TargetHash: fileSHA256([]byte(newContent)),
		At:         time.Now().UTC(),
	}
	if err := saveAiEditUndo(s.dataDir, undo); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "文件已写回，但撤销记录保存失败: " + err.Error()})
		return
	}

	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	if latest, lerr := loadConversation(s.dataDir, c.ID); lerr == nil {
		c = latest
	}
	c.appendMessage("system", kindNotice,
		fmt.Sprintf("AI 已按块写回 %s（接受 %d 块，拒绝 %d 块）。", rel, applied, rejected),
		"", "ai_edit_applied", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, aiEditApplyResponse{
		Path:       rel,
		Applied:    applied,
		Rejected:   rejected,
		NewContent: newContent,
	})
}

// aiEditRegion is the resolved byte/line span of one inline edit.
type aiEditRegion struct {
	StartLine   int
	EndLine     int
	InsertLine  int
	StartOffset int
	EndOffset   int
	OldText     string
	Truncated   bool
}

// aiEditRegionFor resolves a selection against the file content. An empty
// selection is an error rather than a silent whole-file rewrite: getting the
// range wrong is the one failure mode that loses user code.
func aiEditRegionFor(content string, sel aiEditSelection) (aiEditRegion, error) {
	offsets := lineStartOffsets(content)
	lineCount := len(offsets) - 1 // offsets has a trailing len(content)

	if sel.InsertLine > 0 {
		if sel.InsertLine > lineCount+1 {
			return aiEditRegion{}, fmt.Errorf("insert_line %d is beyond the file (%d lines)", sel.InsertLine, lineCount)
		}
		at := offsets[sel.InsertLine-1]
		return aiEditRegion{
			InsertLine:  sel.InsertLine,
			StartLine:   sel.InsertLine,
			EndLine:     sel.InsertLine - 1,
			StartOffset: at,
			EndOffset:   at,
		}, nil
	}

	if sel.StartLine <= 0 || sel.EndLine < sel.StartLine {
		return aiEditRegion{}, fmt.Errorf("invalid selection: start_line=%d end_line=%d", sel.StartLine, sel.EndLine)
	}
	if sel.EndLine > lineCount {
		return aiEditRegion{}, fmt.Errorf("selection ends at line %d but the file has %d lines", sel.EndLine, lineCount)
	}
	start := offsets[sel.StartLine-1]
	end := offsets[sel.EndLine]
	return aiEditRegion{
		StartLine:   sel.StartLine,
		EndLine:     sel.EndLine,
		StartOffset: start,
		EndOffset:   end,
		OldText:     content[start:end],
	}, nil
}

// lineStartOffsets returns the byte offset of every line start plus
// len(content), so line L spans [offsets[L-1], offsets[L]).
func lineStartOffsets(content string) []int {
	out := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && i+1 <= len(content) {
			out = append(out, i+1)
		}
	}
	if out[len(out)-1] != len(content) {
		out = append(out, len(content))
	}
	return out
}

func aiEditMaxTokens(region aiEditRegion) int {
	lines := region.EndLine - region.StartLine + 1
	if lines < 1 {
		lines = 1
	}
	n := lines*80 + 512
	if n < 1024 {
		n = 1024
	}
	if n > 8000 {
		n = 8000
	}
	return n
}

// aiEditTimeout bounds one inline edit. The conversation's RequestTimeout wins
// when set; otherwise 180s is long enough for a slow local model and short
// enough that a hung request does not pin the dialog forever.
func aiEditTimeout(cfg agent.Config) time.Duration {
	if cfg.RequestTimeout > 0 {
		return cfg.RequestTimeout
	}
	return 180 * time.Second
}

// buildAiEditPrompt renders the system+user messages. The whole file is sent
// when it fits, because an inline edit that cannot see the types and helpers
// around it produces plausible garbage.
func buildAiEditPrompt(rel, content string, region aiEditRegion, instruction string) []llm.Message {
	lineCount := len(lineStartOffsets(content)) - 1
	body, truncated := aiEditContextBlock(content, region)

	system := "你是编辑器内的代码改写引擎。用户选中了一个行范围并给出改写指令，你输出该范围的替换代码。\n" +
		"规则：\n" +
		"1. 只输出替换后的代码本身。不要解释、不要前言、不要总结、不要 Markdown 代码围栏。\n" +
		"2. 只输出被选中的那段范围，不要输出整个文件，也不要重复范围之外的代码。\n" +
		"3. 保持文件原有缩进、引号风格和换行风格。\n" +
		"4. 除指令要求的改动外，尽量保持原代码不变；不需要改动时原样输出选区代码。\n" +
		"5. 不要编造不存在的函数或依赖；需要上下文时优先复用文件中已有的定义。"

	target := fmt.Sprintf("选中行范围：%d-%d（共 %d 行）", region.StartLine, region.EndLine, region.EndLine-region.StartLine+1)
	if region.InsertLine > 0 {
		target = fmt.Sprintf("插入位置：第 %d 行之前（不是替换，输出要插入的新代码）", region.InsertLine)
	}

	user := fmt.Sprintf("文件：%s（共 %d 行）\n%s\n", rel, lineCount, target)
	if truncated {
		user += "注意：文件较大，下面只给出选区附近的窗口。\n"
	}
	user += "\n" + body + "\n改写指令：" + instruction + "\n\n只输出替换后的代码："
	return []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}
}

// aiEditContextBlock numbers the lines so the model can anchor itself. Small
// files are sent whole; large ones get a window with the selection marked.
func aiEditContextBlock(content string, region aiEditRegion) (string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	from, to := 1, len(lines)
	truncated := false
	if len(content) > aiEditContextChars {
		truncated = true
		if region.InsertLine > 0 {
			from = region.InsertLine - aiEditWindowLines
			to = region.InsertLine + aiEditWindowLines
		} else {
			from = region.StartLine - aiEditWindowLines
			to = region.EndLine + aiEditWindowLines
		}
		if from < 1 {
			from = 1
		}
		if to > len(lines) {
			to = len(lines)
		}
	}
	var b strings.Builder
	for i := from; i <= to; i++ {
		marker := "  "
		selected := false
		if region.InsertLine > 0 {
			selected = i == region.InsertLine
		} else {
			selected = i >= region.StartLine && i <= region.EndLine
		}
		if selected {
			marker = ">>"
		}
		fmt.Fprintf(&b, "%s %4d | %s\n", marker, i, lines[i-1])
	}
	if truncated {
		fmt.Fprintf(&b, "（窗口 %d-%d，文件共 %d 行）\n", from, to, len(lines))
	}
	return b.String(), truncated
}

// stripCodeFence removes a ```lang fence that weak models add even when told
// not to. It only strips a fence that wraps the entire reply.
func stripCodeFence(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "```") {
		return raw
	}
	firstNL := strings.IndexByte(text, '\n')
	if firstNL < 0 {
		return raw
	}
	last := strings.LastIndex(text, "```")
	if last <= firstNL {
		return raw
	}
	inner := text[firstNL+1 : last]
	if strings.TrimSpace(text[last+3:]) != "" {
		// Content after the closing fence: the fence did not wrap everything.
		return raw
	}
	return inner
}

// aiEditDiff diffs the model's replacement against the region it replaces and
// shifts the line numbers to file coordinates, so the dialog shows the same
// line numbers the editor gutter shows.
func aiEditDiff(oldText, newText string, startLine int) ([]diffHunk, string) {
	lines, _ := computeDiffLines(oldText, newText)
	hunks := toDiffHunks(lines, hunkContextRadius)
	if startLine > 1 {
		shift := startLine - 1
		for i := range hunks {
			for j := range hunks[i].Lines {
				l := &hunks[i].Lines[j]
				if l.OldNo != nil {
					v := *l.OldNo + shift
					l.OldNo = &v
				}
				if l.NewNo != nil {
					v := *l.NewNo + shift
					l.NewNo = &v
				}
			}
		}
	}
	return hunks, aiEditPreview(hunks)
}

func aiEditPreview(hunks []diffHunk) string {
	var b strings.Builder
	for i, h := range hunks {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "@@ 块 %d @@\n", h.Index+1)
		for _, l := range h.Lines {
			switch l.Type {
			case "add":
				b.WriteString("+" + l.Text + "\n")
			case "remove":
				b.WriteString("-" + l.Text + "\n")
			default:
				b.WriteString(" " + l.Text + "\n")
			}
		}
	}
	return b.String()
}

func redactDiffHunks(hunks []diffHunk, secrets []string) []diffHunk {
	if len(secrets) == 0 {
		return hunks
	}
	out := make([]diffHunk, len(hunks))
	for i, h := range hunks {
		lines := make([]diffLine, len(h.Lines))
		copy(lines, h.Lines)
		for j := range lines {
			lines[j].Text = Redact(lines[j].Text, secrets...)
		}
		out[i] = diffHunk{Index: h.Index, Lines: lines, lo: h.lo, hi: h.hi}
	}
	return out
}

// writeAiEditEvent writes one SSE frame. Errors are ignored: the client may
// have navigated away mid-generation, and that is not a server fault.
func writeAiEditEvent(w http.ResponseWriter, f http.Flusher, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	f.Flush()
}
