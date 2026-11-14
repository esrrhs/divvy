package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/esrrhs/divvy/pkg/tools"
)

// aiEditUndoRecord is the snapshot needed to reverse one inline edit in a
// conversation.
//
// It is deliberately NOT stored in the turn rollback journal: that journal is
// consumed by turn rollback, which is keyed on a session id. An inline edit has
// no turn, so a journal entry there would be silently "recovered" by the next
// unrelated rollback. A separate stack keeps undo honest and scoped to the
// exact writes it belongs to.
type aiEditUndoRecord struct {
	Version    int       `json:"version"`
	ConvID     string    `json:"conv_id"`
	WorkDir    string    `json:"workdir"`
	Path       string    `json:"path"`
	Restore    string    `json:"restore"`
	BeforeHash string    `json:"before_hash"`
	TargetHash string    `json:"target_hash"`
	At         time.Time `json:"at"`
}

const (
	aiEditUndoVersion = 2
	// aiEditUndoMaxDepth bounds the stack. An inline edit is cheap to repeat,
	// so a deep stack is not the concern; the concern is a file that grows
	// without bound in the data dir.
	aiEditUndoMaxDepth = 10
)

// aiEditUndoStack is the on-disk shape: the records of the inline edits of one
// conversation, oldest first.
//
// The stack exists because one slot was too little: two inline edits in a row
// left the first one unundoable, and the only way back was to hand-edit the
// file. An editor's Cmd-Z reaches further than that.
type aiEditUndoStack struct {
	Version int                 `json:"version"`
	Records []*aiEditUndoRecord `json:"records"`
}

func aiEditUndoPath(dataDir, convID string) string {
	return filepath.Join(dataDir, "conversations", convID+".ai-edit-undo.json")
}

// saveAiEditUndo appends a record to the conversation's undo stack.
func saveAiEditUndo(dataDir string, rec *aiEditUndoRecord) error {
	stack, err := loadAiEditUndoStack(dataDir, rec.ConvID)
	if err != nil {
		return err
	}
	if stack == nil {
		stack = &aiEditUndoStack{Version: aiEditUndoVersion}
	}
	stack.Version = aiEditUndoVersion
	stack.Records = append(stack.Records, rec)
	if len(stack.Records) > aiEditUndoMaxDepth {
		stack.Records = stack.Records[len(stack.Records)-aiEditUndoMaxDepth:]
	}
	return writeAiEditUndoStack(dataDir, rec.ConvID, stack)
}

func writeAiEditUndoStack(dataDir, convID string, stack *aiEditUndoStack) error {
	data, err := json.MarshalIndent(stack, "", "  ")
	if err != nil {
		return err
	}
	path := aiEditUndoPath(dataDir, convID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadAiEditUndoStack reads the undo stack of a conversation.
//
// A stack written by the previous version (a single record) is migrated on
// read: the user should not lose their undo because the binary was updated.
func loadAiEditUndoStack(dataDir, convID string) (*aiEditUndoStack, error) {
	raw, err := os.ReadFile(aiEditUndoPath(dataDir, convID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var old aiEditUndoRecord
	if err := json.Unmarshal(raw, &old); err == nil && old.Version == 1 && old.Path != "" {
		return &aiEditUndoStack{Version: aiEditUndoVersion, Records: []*aiEditUndoRecord{&old}}, nil
	}
	var stack aiEditUndoStack
	if err := json.Unmarshal(raw, &stack); err != nil {
		return nil, fmt.Errorf("撤销记录损坏：%w", err)
	}
	if stack.Version != aiEditUndoVersion {
		return nil, fmt.Errorf("撤销记录格式不支持")
	}
	return &stack, nil
}

// popAiEditUndo removes the newest record from the stack and returns it.
func popAiEditUndo(dataDir, convID string) (*aiEditUndoRecord, *aiEditUndoStack, error) {
	stack, err := loadAiEditUndoStack(dataDir, convID)
	if err != nil || stack == nil || len(stack.Records) == 0 {
		return nil, stack, err
	}
	rec := stack.Records[len(stack.Records)-1]
	stack.Records = stack.Records[:len(stack.Records)-1]
	return rec, stack, nil
}

type aiEditUndoResponse struct {
	Path       string `json:"path"`
	Restored   bool   `json:"restored"`
	NewContent string `json:"new_content"`
	// Remaining counts the writes that can still be undone after this one.
	Remaining int `json:"remaining"`
}

// handleAiEditUndo reverses the last inline edit, but only if the file still
// holds exactly what that edit wrote. If anything else touched the file in the
// meantime, a blind restore would destroy that work, so it refuses.
func (s *Server) handleAiEditUndo(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	c, err := s.loadConv(convID)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	rec, stack, err := popAiEditUndo(s.dataDir, c.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有可撤销的 AI 改写"})
		return
	}

	unlock, lerr := s.mgr.BeginWorkdirWrite(c.WorkDir, "ai-edit-undo:"+c.ID+":"+rec.Path)
	if lerr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前工作区正被另一任务占用，不能撤销：" + lerr.Error()})
		return
	}
	defer unlock()

	sb, err := tools.NewSandbox(c.WorkDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	content, err := sb.ReadFile(rec.Path)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "文件已不存在或不可读，无法撤销：" + err.Error()})
		return
	}
	if fileSHA256([]byte(content)) != rec.TargetHash {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":         "文件在这次 AI 改写之后又被修改过，撤销会覆盖那些改动，已停止",
			"conflict_path": rec.Path,
		})
		return
	}
	if err := sb.WriteFile(rec.Path, rec.Restore); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// The record is dropped only after the restore actually happened: a failed
	// undo must stay undoable.
	if err := writeAiEditUndoStack(s.dataDir, c.ID, stack); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "文件已恢复，但撤销记录保存失败: " + err.Error()})
		return
	}

	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	if latest, lerr := loadConversation(s.dataDir, c.ID); lerr == nil {
		c = latest
	}
	c.appendMessage("system", kindNotice,
		fmt.Sprintf("已撤销上一次 AI 改写，%s 恢复到写回前的内容。", rec.Path),
		"", "ai_edit_undone", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, aiEditUndoResponse{
		Path:       rec.Path,
		Restored:   true,
		NewContent: rec.Restore,
		Remaining:  len(stack.Records),
	})
}
