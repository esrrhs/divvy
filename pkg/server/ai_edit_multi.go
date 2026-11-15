package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Multi-file inline edit (Cmd-K over several files).
//
// The model is still asked for CODE ONLY, once per file. Asking a weak model
// for a JSON list of {path, lines, code} is exactly the structured-output
// failure the single-file protocol was designed to avoid: one malformed reply
// would throw away the whole multi-file round. Instead the server owns the
// file list (the user picked it), sends the same instruction to each file,
// and gives every file its own hash-guarded proposal.
//
// The write side is all-or-nothing: every file is re-read and checked BEFORE
// the first write. A conflict on the third file must not leave the first two
// half-rewritten. If a write itself fails midway, already-written files are
// compensated back to their original content, and the undo stack gets one
// entry covering the whole round.

const (
	aiEditMaxTargets     = 6
	aiEditSiblingChars   = 6000
	aiEditSiblingPerFile = 2000
)

// aiEditWriteHook is a deterministic test hook: it runs just before a file is
// written, so the compensation path can be tested without a broken disk.
var aiEditWriteHook func(path string) error

func normalizeAiEditTargets(req aiEditRequest) ([]aiEditTarget, error) {
	if strings.TrimSpace(req.Instruction) == "" {
		return nil, fmt.Errorf("instruction is required")
	}
	targets := req.Targets
	if len(targets) == 0 {
		targets = []aiEditTarget{{Path: req.Path, Selection: req.Selection}}
	}
	if len(targets) > aiEditMaxTargets {
		return nil, fmt.Errorf("一次最多改写 %d 个文件，收到 %d 个", aiEditMaxTargets, len(targets))
	}
	out := make([]aiEditTarget, 0, len(targets))
	seen := map[string]bool{}
	for i, t := range targets {
		t.Path = strings.TrimSpace(t.Path)
		if t.Path == "" {
			return nil, fmt.Errorf("第 %d 个文件缺少 path", i+1)
		}
		key := filepath.Clean(t.Path)
		if seen[key] {
			return nil, fmt.Errorf("同一个文件被列了两次：%s", t.Path)
		}
		seen[key] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("path is required")
	}
	return out, nil
}

// aiEditResponseFromProposals fills the legacy top-level fields from the
// first proposal, so an older client sees exactly the old single-file shape.
func aiEditResponseFromProposals(props []aiEditProposal) aiEditResponse {
	out := aiEditResponse{Proposals: props}
	if len(props) == 0 {
		return out
	}
	p := props[0]
	out.Path = p.Path
	out.BaseHash = p.BaseHash
	out.StartLine = p.StartLine
	out.EndLine = p.EndLine
	out.InsertLine = p.InsertLine
	out.OldText = p.OldText
	out.NewText = p.NewText
	out.Hunks = p.Hunks
	out.Preview = p.Preview
	out.ContextBefore = p.ContextBefore
	out.ContextAfter = p.ContextAfter
	out.Truncated = p.Truncated
	return out
}

// aiEditPendingCode returns the most recent proposal for `rel` and whether it
// is still pending (not written to disk).
//
// Scanning BACKWARDS matters once a round has several files: the last turn
// overall is about another file. Also, once the latest entry for this file is
// marked applied, an older pending entry must NOT be revived — the file on
// disk already supersedes it.
func aiEditPendingCode(history []aiEditTurn, rel, primary string) (string, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		t := history[i]
		if len(t.Files) > 0 {
			for j := len(t.Files) - 1; j >= 0; j-- {
				f := t.Files[j]
				if aiEditTurnPathMatches(f.Path, rel, primary) {
					return f.Code, !f.Applied
				}
			}
			continue
		}
		if aiEditTurnPathMatches(t.Path, rel, primary) {
			return t.Code, !t.Applied
		}
	}
	return "", false
}

// An empty path is the older client's "same file". With several files in a
// round it can only mean the primary one, not every file at once.
func aiEditTurnPathMatches(path, rel, primary string) bool {
	if path == "" {
		return rel == primary
	}
	return filepath.Clean(path) == filepath.Clean(rel)
}

// aiEditPrepared is one target after the file has been read and the selection
// resolved. Everything the model needs is computed before any LLM call, so a
// bad path or selection fails the whole request instead of leaving a partial
// set of proposals.
type aiEditPrepared struct {
	rel           string
	region        aiEditRegion
	promptContent string
	promptRegion  aiEditRegion
	messages      []llm.Message
	proposal      aiEditProposal
}

// aiEditSiblingContext describes the OTHER files in the round. "Rename X and
// update its callers" only works if the file handling X can see the callers.
// The text is bounded: a large selection must not blow up every prompt.
func aiEditSiblingContext(prepared []aiEditPrepared, self int) string {
	var b strings.Builder
	used := 0
	for i, p := range prepared {
		if i == self || used >= aiEditSiblingChars {
			continue
		}
		region := p.region.OldText
		if len(region) > aiEditSiblingPerFile {
			region = region[:aiEditSiblingPerFile] + "\n…（已截断）"
		}
		where := fmt.Sprintf("%d-%d 行", p.region.StartLine, p.region.EndLine)
		if p.region.InsertLine > 0 {
			where = fmt.Sprintf("第 %d 行前插入", p.region.InsertLine)
			region = "(插入位置，无原代码)"
		}
		block := fmt.Sprintf("\n--- %s（%s）---\n%s\n", p.rel, where, region)
		if used+len(block) > aiEditSiblingChars {
			block = block[:aiEditSiblingChars-used]
		}
		b.WriteString(block)
		used += len(block)
	}
	return b.String()
}

// handleAiEditMulti generates one proposal per target. Calls run concurrently
// (a six-file round should not take six times as long); with SSE the client
// gets per-file progress events instead of a mixed-up delta stream.
func (s *Server) handleAiEditMulti(w http.ResponseWriter, c *Conversation, req aiEditRequest, targets []aiEditTarget) {
	sb, err := tools.NewSandbox(c.WorkDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	secrets := s.secretsFor(c.ID)
	history := req.History
	if len(history) > aiEditMaxHistoryTurns {
		history = history[len(history)-aiEditMaxHistoryTurns:]
	}

	// Resolve every path first: primary is the reference for empty-path
	// history entries from older clients.
	rels := make([]string, len(targets))
	for i, t := range targets {
		_, rel, err := resolveProjectPath(c.WorkDir, t.Path)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		rels[i] = rel
	}
	primary := rels[0]

	prepared := make([]aiEditPrepared, len(targets))
	for i, t := range targets {
		content, err := sb.ReadFile(rels[i])
		if err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found: " + rels[i]})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		region, err := aiEditRegionFor(content, t.Selection)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rels[i] + ": " + err.Error()})
			return
		}
		promptContent, promptRegion := content, region
		if code, pending := aiEditPendingCode(history, rels[i], primary); pending {
			virtual, vregion, verr := aiEditVirtualRegion(content, region, code)
			if verr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": rels[i] + ": " + verr.Error()})
				return
			}
			promptContent, promptRegion = virtual, vregion
		}
		before, after := aiEditContext(content, region, aiEditContextLines)
		prepared[i] = aiEditPrepared{
			rel:           rels[i],
			region:        region,
			promptContent: promptContent,
			promptRegion:  promptRegion,
			proposal: aiEditProposal{
				Path:          rels[i],
				BaseHash:      fileSHA256([]byte(content)),
				StartLine:     region.StartLine,
				EndLine:       region.EndLine,
				InsertLine:    region.InsertLine,
				OldText:       Redact(region.OldText, secrets...),
				ContextBefore: Redact(before, secrets...),
				ContextAfter:  Redact(after, secrets...),
				Truncated:     region.Truncated,
			},
		}
	}
	for i := range prepared {
		siblings := aiEditSiblingContext(prepared, i)
		prepared[i].messages = buildAiEditPrompt(prepared[i].rel, prepared[i].promptContent,
			prepared[i].promptRegion, req.Instruction, history, siblings)
	}

	cfg, err := s.configForConversation(c)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	client := s.newClient(cfg)

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

	ctx, cancel := context.WithTimeout(context.Background(), aiEditTimeout(cfg))
	defer cancel()
	props := make([]aiEditProposal, len(prepared))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for i := range prepared {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &prepared[i]
			prop := p.proposal
			request := llm.Request{
				Model:       cfg.Model,
				Messages:    p.messages,
				Temperature: 0.1,
				MaxTokens:   aiEditMaxTokens(p.promptRegion),
			}
			resp, err := client.Chat(ctx, request)
			if err != nil {
				prop.Error = "LLM 调用失败: " + err.Error()
			} else if resp == nil || strings.TrimSpace(resp.Content) == "" {
				prop.Error = "模型返回了空回复，这个文件未改动"
			} else {
				newText := stripCodeFence(resp.Content)
				hunks, preview := aiEditDiff(p.region.OldText, newText, p.region.StartLine)
				prop.NewText = Redact(newText, secrets...)
				prop.Hunks = redactDiffHunks(hunks, secrets)
				prop.Preview = Redact(preview, secrets...)
			}
			if prop.Error != "" {
				// A failed file stays untouched: applying it must be a no-op,
				// never "replace the region with an empty string".
				prop.NewText = prop.OldText
				prop.Hunks = nil
				prop.Preview = ""
			}
			mu.Lock()
			props[i] = prop
			if stream {
				done++
				writeAiEditEvent(w, flusher, "progress", map[string]any{
					"done": done, "total": len(prepared), "path": p.rel,
					"error": prop.Error,
				})
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	out := aiEditResponseFromProposals(props)
	if stream {
		writeAiEditEvent(w, flusher, "done", out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func aiEditApplyItems(req aiEditApplyRequest) ([]aiEditApplyProposal, error) {
	if len(req.Proposals) > 0 {
		if len(req.Proposals) > aiEditMaxTargets {
			return nil, fmt.Errorf("一次最多写回 %d 个文件，收到 %d 个", aiEditMaxTargets, len(req.Proposals))
		}
		return req.Proposals, nil
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	if strings.TrimSpace(req.BaseHash) == "" {
		return nil, fmt.Errorf("base_hash is required: 需要它判断文件是否在你编辑期间被改动过")
	}
	return []aiEditApplyProposal{{
		Path:       req.Path,
		BaseHash:   req.BaseHash,
		StartLine:  req.StartLine,
		EndLine:    req.EndLine,
		InsertLine: req.InsertLine,
		OldText:    req.OldText,
		NewText:    req.NewText,
		Decisions:  req.Decisions,
	}}, nil
}

type aiEditApplyPrepared struct {
	item       aiEditApplyProposal
	rel        string
	content    string
	region     aiEditRegion
	newContent string
	hunks      []diffHunk
	applied    int
	rejected   int
}

// applyAiEditProposals validates every file, then writes them as one round.
func (s *Server) applyAiEditProposals(w http.ResponseWriter, r *http.Request, items []aiEditApplyProposal) {
	convID := r.PathValue("id")
	c, err := s.loadConv(convID)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	unlock, lerr := s.mgr.BeginWorkdirWrite(c.WorkDir, "ai-edit:"+c.ID)
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

	prepared := make([]aiEditApplyPrepared, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		path := strings.TrimSpace(item.Path)
		if path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
			return
		}
		if strings.TrimSpace(item.BaseHash) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_hash is required: 需要它判断文件是否在你编辑期间被改动过"})
			return
		}
		_, rel, err := resolveProjectPath(c.WorkDir, path)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if seen[rel] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "同一个文件被列了两次：" + rel})
			return
		}
		seen[rel] = true
		content, err := sb.ReadFile(rel)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found: " + rel})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		if got := fileSHA256([]byte(content)); got != strings.TrimSpace(item.BaseHash) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":         "文件在你生成改动之后被修改过，已停止写回，请重新生成",
				"conflict_path": rel,
			})
			return
		}
		region, err := aiEditRegionFor(content, aiEditSelection{
			StartLine: item.StartLine, EndLine: item.EndLine, InsertLine: item.InsertLine,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rel + ": " + err.Error()})
			return
		}
		if region.OldText != item.OldText {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":         "选中的代码与生成时不一致（文件可能已被外部修改），已停止写回",
				"conflict_path": rel,
			})
			return
		}
		replacement, hunks, err := applyHunkDecisions(region.OldText, item.NewText, item.Decisions)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rel + ": " + err.Error()})
			return
		}
		if region.InsertLine > 0 {
			if replacement != "" && !strings.HasSuffix(replacement, "\n") {
				replacement += "\n"
			}
			if region.StartOffset > 0 && content[region.StartOffset-1] != '\n' {
				replacement = "\n" + replacement
			}
		}
		applied, rejected := 0, 0
		for i := range hunks {
			if i < len(item.Decisions) && !item.Decisions[i] {
				rejected++
			} else {
				applied++
			}
		}
		prepared = append(prepared, aiEditApplyPrepared{
			rel:        rel,
			content:    content,
			newContent: content[:region.StartOffset] + replacement + content[region.EndOffset:],
			applied:    applied,
			rejected:   rejected,
		})
	}

	// Nothing was written during validation. From here on a failure must be
	// compensated: a half-applied multi-file round is worse than a refused
	// one, because the user would have no single undo step to get back.
	var written []aiEditApplyPrepared
	writeOne := func(p aiEditApplyPrepared) error {
		if aiEditWriteHook != nil {
			if err := aiEditWriteHook(p.rel); err != nil {
				return err
			}
		}
		return sb.WriteFile(p.rel, p.newContent)
	}
	for _, p := range prepared {
		if p.newContent == p.content {
			continue // all decisions rejected (or no changes proposed)
		}
		if err := writeOne(p); err != nil {
			msg := "写回 " + p.rel + " 失败：" + err.Error()
			if cerr := compensateAiEditWrites(sb, written); cerr != nil {
				msg += "；且回滚已写文件失败：" + cerr.Error() + "（请手动检查这些文件）"
			} else if len(written) > 0 {
				msg += "；已把本次其它文件恢复原状"
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
			return
		}
		written = append(written, p)
	}

	out := aiEditApplyResponse{}
	for _, p := range prepared {
		out.Applied += p.applied
		out.Rejected += p.rejected
		out.Files = append(out.Files, aiEditApplyResult{
			Path: p.rel, Applied: p.applied, Rejected: p.rejected, NewContent: p.newContent,
		})
	}
	if len(prepared) > 0 {
		out.Path = prepared[0].rel
		out.NewContent = prepared[0].newContent
	}
	if len(written) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	for _, p := range written {
		out.Paths = append(out.Paths, p.rel)
	}

	// One undo step for the WHOLE round. The legacy fields describe the first
	// file so older clients can still read the record; Files is the full set.
	undo := &aiEditUndoRecord{
		Version:    aiEditUndoVersion,
		ConvID:     c.ID,
		WorkDir:    c.WorkDir,
		Path:       written[0].rel,
		Restore:    written[0].content,
		BeforeHash: fileSHA256([]byte(written[0].content)),
		TargetHash: fileSHA256([]byte(written[0].newContent)),
		At:         time.Now().UTC(),
	}
	for _, p := range written {
		undo.Files = append(undo.Files, aiEditUndoFile{
			Path:       p.rel,
			Restore:    p.content,
			BeforeHash: fileSHA256([]byte(p.content)),
			TargetHash: fileSHA256([]byte(p.newContent)),
		})
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
		fmt.Sprintf("AI 已按块写回 %s（共 %d 个文件，接受 %d 块，拒绝 %d 块）。",
			strings.Join(out.Paths, "、"), len(out.Paths), out.Applied, out.Rejected),
		"", "ai_edit_applied", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// compensateAiEditWrites restores already-written files, newest first. The
// original contents are in memory, so this needs no extra journal.
func compensateAiEditWrites(sb *tools.Sandbox, written []aiEditApplyPrepared) error {
	var first error
	for i := len(written) - 1; i >= 0; i-- {
		if err := sb.WriteFile(written[i].rel, written[i].content); err != nil && first == nil {
			first = err
		}
	}
	return first
}
