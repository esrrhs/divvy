package server

import (
	"fmt"
	"net/http"
	"os"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/tools"
)

func (s *Server) handleRollbackTurn(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !req.Confirm {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm must be true to undo this turn's files"})
		return
	}

	unlock, lerr := s.mgr.BeginWorkdirWriteExclusive(c.WorkDir, "rollback:"+c.ID+":"+sid)
	if lerr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前工作区正被另一任务或写入占用，不能撤销：" + lerr.Error()})
		return
	}
	defer unlock()

	if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
		c = latest
	}
	if containsString(c.RolledBackTurns, sid) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这一轮的修改已经撤销过了"})
		return
	}
	if ok, reason := s.turnRollbackAllowed(c); !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
		return
	}

	if j, jerr := loadRollbackJournal(s.dataDir, c.ID); jerr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        journalUnreadableMessage(jerr.Error()),
			"conversation": s.viewConversation(c),
		})
		return
	} else if j != nil {
		out, _ := s.recoverRollbackJournal(j, false)
		if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
			c = latest
		}
		if out != nil && out.Completed && containsString(c.RolledBackTurns, sid) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":           true,
				"session_id":   sid,
				"restored":     out.Restored,
				"conversation": s.viewConversation(c),
			})
			return
		}
	}
	// Always consult the shared workdir barrier, including when this
	// conversation has no journal of its own. A sibling pending or
	// unreadable journal must block a new rollback just like any other write.
	if reason, blocked := s.rollbackWriteBlock(c); blocked {
		writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
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
	if len(changes) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这一轮没有可撤销的文件改动"})
		return
	}

	var plan []rollbackOp
	for _, ch := range changes {
		op, perr := planRollbackOp(c.WorkDir, ch)
		if perr != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": perr.Error()})
			return
		}
		if op.skip {
			continue
		}
		plan = append(plan, op)
	}
	if len(plan) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有需要撤销的文件（工作区已是该轮开始前的状态）"})
		return
	}

	out, aerr := s.applyJournaledRollback(c, sid, plan)
	s.writeRollbackOutcome(w, c, sid, out, aerr)
}

func (s *Server) handleRecoverRollback(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	j, err := loadRollbackJournal(s.dataDir, c.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":           false,
			"error":        journalUnreadableMessage(err.Error()),
			"pending":      true,
			"conversation": s.viewConversation(c),
		})
		return
	}
	if j == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"recovered":    false,
			"conversation": s.viewConversation(c),
		})
		return
	}
	out, rerr := s.recoverRollbackJournal(j, true)
	if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
		c = latest
	}
	status := http.StatusOK
	payload := map[string]any{
		"ok":           rerr == nil && (out == nil || !out.Pending),
		"conversation": s.viewConversation(c),
	}
	if out != nil {
		payload["completed"] = out.Completed
		payload["compensated"] = out.Compensated
		payload["pending"] = out.Pending
		payload["restored"] = out.Restored
		payload["conflicts"] = out.Conflicts
		if out.Message != "" {
			payload["error"] = out.Message
		}
	}
	if rerr != nil {
		status = http.StatusConflict
		if _, ok := payload["error"]; !ok {
			payload["error"] = rerr.Error()
		}
		payload["ok"] = false
	}
	writeJSON(w, status, payload)
}

func (s *Server) writeRollbackOutcome(w http.ResponseWriter, c *Conversation, sid string, out *rollbackOutcome, aerr error) {
	if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
		c = latest
	}
	if aerr == nil && out != nil && out.Completed {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"session_id":   sid,
			"restored":     out.Restored,
			"conversation": s.viewConversation(c),
		})
		return
	}
	msg := "撤销失败"
	if aerr != nil {
		msg = aerr.Error()
	} else if out != nil && out.Message != "" {
		msg = out.Message
	}
	// A mid-rollback failure that left a journal, compensated cleanly, or
	// hit an external edit is recoverable client state, not a bare 500.
	status := http.StatusConflict
	if out == nil {
		status = http.StatusInternalServerError
	}
	body := map[string]any{"error": msg, "conversation": s.viewConversation(c)}
	if out != nil {
		body["pending"] = out.Pending
		body["compensated"] = out.Compensated
		body["conflicts"] = out.Conflicts
	}
	writeJSON(w, status, body)
}

func (s *Server) turnRollbackAllowed(c *Conversation) (bool, string) {
	phase, _ := s.conversationRun(c)
	if phase == string(agent.PhaseLeafApproval) {
		return false, "当前正在审批，不能撤销轮次修改，以免和待合并的改动冲突"
	}
	if activeRunPhase(phase) {
		return false, "当前轮还在执行，不能撤销已有轮次的文件"
	}
	if phase == string(agent.PhasePaused) {
		return false, "任务已暂停且本轮尚未结束。请先续跑或结束后，再撤销已完成轮次的修改"
	}
	return true, ""
}

type rollbackOp struct {
	path        string
	restore     string
	delete      bool // file did not exist before this turn
	skip        bool
	beforeHash  string // digest verified at plan time; prepare must still see it
	restoreMode os.FileMode
}

func planRollbackOp(workdir string, ch turnChange) (rollbackOp, error) {
	if _, _, err := resolveProjectPath(workdir, ch.Path); err != nil {
		return rollbackOp{}, err
	}
	pre, existed, okPre := preTurnContent(ch)
	if !okPre {
		return rollbackOp{}, fmt.Errorf("无法安全撤销 %s：该轮快照未保存该文件在本轮开始前的完整内容（过大或为二进制）。未改动任何文件", ch.Path)
	}
	post := postTurnContent(ch)

	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return rollbackOp{}, err
	}
	cur, err := sb.ReadFile(ch.Path)
	exists := err == nil
	if err != nil {
		if !isNotExistish(err) {
			return rollbackOp{}, err
		}
		exists = false
		cur = ""
	}

	var curMode uint32
	if exists {
		mode, merr := workdirFilePermBits(workdir, ch.Path)
		if merr != nil {
			return rollbackOp{}, merr
		}
		curMode = mode
	}
	contentPre := (!exists && !existed) || (exists && existed && cur == pre)
	modePre := true
	if exists && ch.OldMode != 0 {
		modePre = curMode == ch.OldMode
	}
	if contentPre && modePre {
		return rollbackOp{path: ch.Path, skip: true}, nil
	}
	matchesPost := (!exists && post == "") || (exists && cur == post)
	if matchesPost && recordedTurnModeChange(ch) && exists && curMode != ch.NewMode {
		matchesPost = false
	}
	if !matchesPost {
		return rollbackOp{}, fmt.Errorf("工作区中的 %s 已与本轮结果不一致（可能被后续轮次、外部编辑或按块写回改过）。为避免覆盖这些改动，已停止撤销，未改动任何文件", ch.Path)
	}
	op := rollbackOp{path: ch.Path, restore: pre, delete: !existed, beforeHash: fileDigest(exists, cur)}
	if ch.OldMode != 0 {
		op.restoreMode = os.FileMode(ch.OldMode).Perm()
	}
	return op, nil
}

// recordedTurnModeChange reports a snapshot that captured both sides of a
// permission change. A zero OldMode or NewMode is a legacy/unknown record
// and is not treated as a recoverable mode-only edit.
func recordedTurnModeChange(ch turnChange) bool {
	return ch.OldMode != 0 && ch.NewMode != 0 && ch.OldMode != ch.NewMode
}

// preTurnContent is the file body before this turn (empty = did not exist).
// preTurnContent is the file body before this turn. existed is false when
// the file was created by this turn (restore must delete it).
func preTurnContent(ch turnChange) (content string, existed, ok bool) {
	switch ch.Status {
	case "added":
		return "", false, true
	case "deleted", "modified":
		if ch.Truncated && ch.Old == "" {
			return "", false, false
		}
		return ch.Old, true, true
	default:
		if ch.Old == "" && ch.New != "" && !ch.Truncated {
			return "", false, true
		}
		if ch.Truncated && ch.Old == "" {
			return "", false, false
		}
		return ch.Old, ch.Old != "" || ch.Status == "deleted", true
	}
}

// postTurnContent is what the workdir should contain if this turn's result
// (or a later hunk apply of it) is still in place. Empty means absent.
func postTurnContent(ch turnChange) string {
	if ch.Applied != "" || ch.AppliedStatus == "rejected" || ch.AppliedStatus == "accepted" {
		return ch.Applied
	}
	if ch.Status == "deleted" {
		return ""
	}
	return ch.New
}
