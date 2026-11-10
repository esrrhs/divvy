package server

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/tools"
)

type turnChangeView struct {
	turnChange
	Hunks           []diffHunk `json:"hunks,omitempty"`
	HunkCount       int        `json:"hunk_count,omitempty"`
	Editable        bool       `json:"editable"`
	EditBlockReason string     `json:"edit_block_reason,omitempty"`
}

type applyHunkRequest struct {
	Path      string         `json:"path"`
	All       string         `json:"all"`
	Decisions []hunkDecision `json:"decisions"`
}

func viewTurnChanges(changes []turnChange, editable bool, reason string) []turnChangeView {
	out := make([]turnChangeView, 0, len(changes))
	for _, c := range changes {
		v := turnChangeView{turnChange: c, Editable: editable, EditBlockReason: reason}
		if !c.Binary {
			lines, _ := computeDiffLines(c.Old, c.New)
			v.Hunks = toDiffHunks(lines, hunkContextRadius)
			v.HunkCount = len(v.Hunks)
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) turnChangesEditable(c *Conversation, sid string) (bool, string) {
	latest := c.CurrentSessionID
	if latest == "" && len(c.SessionIDs) > 0 {
		latest = c.SessionIDs[len(c.SessionIDs)-1]
	}
	if sid != latest {
		return false, "这是较早轮次的只读回看，不能改写当前工作区"
	}
	if containsString(c.RolledBackTurns, sid) {
		return false, "这一轮的修改已经撤销，不能再按块写回工作区"
	}
	phase, _ := s.conversationRun(c)
	if phase == string(agent.PhaseLeafApproval) {
		return false, "当前叶子正在等待审批。请在中间的叶子审查里按块决定并批准后才会合并到工作区，以免两套审阅互相覆盖"
	}
	if activeRunPhase(phase) {
		return false, "当前轮还在执行，不能把本轮快照写回工作区"
	}
	if phase == string(agent.PhasePaused) {
		return false, "任务已暂停且本轮尚未结束。请先续跑或结束后，再按块改写工作区"
	}
	if reason, blocked := s.rollbackWriteBlock(c); blocked {
		return false, reason
	}
	return true, ""
}

func (s *Server) handleApplyTurnHunks(w http.ResponseWriter, r *http.Request) {
	// Read the body first so a slow client cannot hold a pre-check
	// while a new run starts in the same workdir.
	var req applyHunkRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	sid := r.PathValue("sessionId")
	if !containsString(c.SessionIDs, sid) && c.CurrentSessionID != sid {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "turn not found in this conversation"})
		return
	}
	ok, reason := s.turnChangesEditable(c, sid)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	// Resolve first so a path cannot walk out of the project.
	if _, _, err := resolveProjectPath(c.WorkDir, path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	changes, err := loadTurnChanges(s.dataDir, c.ID, sid)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no stored changes for this turn"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	idx := -1
	for i, ch := range changes {
		if ch.Path == path || strings.TrimPrefix(ch.Path, "./") == strings.TrimPrefix(path, "./") {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not in this turn's changes"})
		return
	}
	ch := changes[idx]
	var result string
	var stored []string
	var status string
	if ch.Binary {
		all := strings.ToLower(strings.TrimSpace(req.All))
		if all != "accept" && all != "reject" && all != "accepted" && all != "rejected" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "二进制文件只能整份接受或拒绝"})
			return
		}
		if all == "accept" || all == "accepted" {
			result, status = ch.New, "accepted"
			stored = []string{"accept"}
		} else {
			result, status = ch.Old, "rejected"
			stored = []string{"reject"}
		}
	} else {
		_, hunks, err := applyHunkDecisions(ch.Old, ch.New, nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		dec, err := parseHunkDecisions(len(hunks), req.Decisions, req.All)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, _, err = applyHunkDecisions(ch.Old, ch.New, dec)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		stored = storeDecisions(dec)
		status = decisionSummary(dec)
	}
	unlock, lerr := s.mgr.BeginWorkdirWrite(c.WorkDir, "hunk:"+c.ID+":"+sid)
	if lerr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前工作区正被另一任务或写入占用，不能回写本轮快照：" + lerr.Error()})
		return
	}
	defer unlock()
	// Re-load and re-check after the lease: a new run may have started
	// while the request body was still arriving.
	if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
		c = latest
	}
	ok, reason = s.turnChangesEditable(c, sid)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
		return
	}
	if err := ensureWorkdirMatchesTurn(c.WorkDir, ch); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := applyTurnResult(c.WorkDir, ch, result, status); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	ch.HunkDecisions = stored
	ch.Applied = result
	ch.AppliedStatus = status
	changes[idx] = ch
	secrets := s.secretsFor(c.ID)
	if err := saveTurnChanges(s.dataDir, c.ID, sid, changes, secrets); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	if latest, lerr := loadConversation(s.dataDir, c.ID); lerr == nil {
		latest.appendMessage("system", kindNotice,
			fmt.Sprintf("已按块更新工作区中的 %s（%s）。这只影响当前工作区，较早轮次的快照仍可只读回看。", ch.Path, applyStatusLabel(status)),
			sid, "hunks_applied", secrets)
		_ = saveConversation(s.dataDir, latest, secrets)
		c = latest
	}
	s.mu.Unlock()
	ok, reason = s.turnChangesEditable(c, sid)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":   sid,
		"path":         ch.Path,
		"applied":      status,
		"changes":      viewTurnChanges(changes, ok, reason),
		"editable":     ok,
		"conversation": s.viewConversation(c),
	})
}

func applyStatusLabel(status string) string {
	switch status {
	case "accepted":
		return "全部接受"
	case "rejected":
		return "全部拒绝"
	default:
		return "部分接受"
	}
}

func ensureWorkdirMatchesTurn(workdir string, ch turnChange) error {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return err
	}
	cur, err := sb.ReadFile(ch.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		// Resolve errors (escape, etc.) should be reported.
		if !os.IsNotExist(err) && !isNotExistish(err) {
			// Missing file is OK for added/deleted cases below.
			if !strings.Contains(strings.ToLower(err.Error()), "no such file") &&
				!strings.Contains(strings.ToLower(err.Error()), "not found") {
				return err
			}
		}
		exists = false
		cur = ""
	}
	if ch.Applied != "" || ch.AppliedStatus == "rejected" {
		if (exists && cur == ch.Applied) || (!exists && ch.Applied == "") {
			return nil
		}
	}
	if exists && (cur == ch.New || cur == ch.Old) {
		return nil
	}
	if !exists && (ch.Status == "added" || ch.New == "") {
		return nil
	}
	return fmt.Errorf("工作区中的 %s 已与本轮快照不一致，拒绝覆盖，以免误改你在审阅之外做的修改", ch.Path)
}

func isNotExistish(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file")
}

func writeTurnResultDelete(workdir, rel, content string, deleteFile bool) error {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return err
	}
	if deleteFile {
		err := sb.DeletePath(rel, false)
		if err != nil && !isNotExistish(err) {
			return err
		}
		return nil
	}
	return sb.WriteFile(rel, content)
}

// applyTurnResult writes or deletes based on the turn change's status, so an
// accepted empty-file edit is not confused with a deletion.
func applyTurnResult(workdir string, ch turnChange, result, appliedStatus string) error {
	return writeTurnResultDelete(workdir, ch.Path, result, resultIsDelete(ch, result, appliedStatus))
}

func resultIsDelete(ch turnChange, result, appliedStatus string) bool {
	if result != "" {
		return false
	}
	st := strings.ToLower(strings.TrimSpace(appliedStatus))
	switch ch.Status {
	case "deleted":
		// Restoring a deleted empty file must write it back, not delete again.
		return st == "accepted" || st == "accept" || st == ""
	case "added":
		// Rejecting an added file (result empty) removes it; accepting an
		// added empty file writes it.
		return st == "rejected" || st == "reject"
	default:
		// Modified (or unknown) files may legitimately become empty.
		return false
	}
}

func patchesFromApprovalHunks(changes []tools.FileChange, reqs map[string]applyHunkRequest) (map[string]string, []string, error) {
	byPath := map[string]tools.FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	patches := map[string]string{}
	var deletes []string
	for path, req := range reqs {
		ch, ok := byPath[path]
		if !ok {
			return nil, nil, fmt.Errorf("file %s is not in this leaf's review set", path)
		}
		if ch.Binary || ch.Truncated {
			all := strings.ToLower(strings.TrimSpace(req.All))
			switch all {
			case "accept", "accepted", "":
				// leave the mirror file as the leaf wrote it
			case "reject", "rejected":
				if ch.Status == tools.ChangeAdded || ch.NewContent == "" && ch.Status != tools.ChangeDeleted {
					deletes = append(deletes, path)
				} else {
					patches[path] = ch.OldContent
				}
			default:
				return nil, nil, fmt.Errorf("binary/truncated file %s can only be accepted or rejected as a whole", path)
			}
			continue
		}
		_, hunks, err := applyHunkDecisions(ch.OldContent, ch.NewContent, nil)
		if err != nil {
			return nil, nil, err
		}
		dec, err := parseHunkDecisions(len(hunks), req.Decisions, req.All)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		result, _, err := applyHunkDecisions(ch.OldContent, ch.NewContent, dec)
		if err != nil {
			return nil, nil, err
		}
		if result == "" && (ch.Status == tools.ChangeAdded || ch.Status == tools.ChangeDeleted && decisionSummary(dec) == "accepted") {
			deletes = append(deletes, path)
			continue
		}
		patches[path] = result
	}
	return patches, deletes, nil
}

// viewApprovalChanges attaches the same hunk list the approve-with-patches
// path will use, so the UI does not invent a different split.
func viewApprovalChanges(changes []tools.FileChange) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		item := map[string]any{
			"path":        c.Path,
			"status":      c.Status,
			"conflict":    c.Conflict,
			"binary":      c.Binary,
			"truncated":   c.Truncated,
			"old_mode":    c.OldMode,
			"new_mode":    c.NewMode,
			"old_content": c.OldContent,
			"new_content": c.NewContent,
		}
		if !c.Binary {
			lines, _ := computeDiffLines(c.OldContent, c.NewContent)
			hunks := toDiffHunks(lines, hunkContextRadius)
			item["hunks"] = hunks
			item["hunk_count"] = len(hunks)
		}
		out = append(out, item)
	}
	return out
}
