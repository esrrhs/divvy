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

// aiEditUndoRecord is the snapshot needed to reverse the most recent inline
// edit in a conversation.
//
// It is deliberately NOT stored in the turn rollback journal: that journal is
// consumed by turn rollback, which is keyed on a session id. An inline edit has
// no turn, so a journal entry there would be silently "recovered" by the next
// unrelated rollback. A separate one-slot record keeps undo honest and scoped
// to the exact write it belongs to.
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

const aiEditUndoVersion = 1

func aiEditUndoPath(dataDir, convID string) string {
	return filepath.Join(dataDir, "conversations", convID+".ai-edit-undo.json")
}

func saveAiEditUndo(dataDir string, rec *aiEditUndoRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := aiEditUndoPath(dataDir, rec.ConvID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadAiEditUndo(dataDir, convID string) (*aiEditUndoRecord, error) {
	raw, err := os.ReadFile(aiEditUndoPath(dataDir, convID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec aiEditUndoRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("撤销记录损坏：%w", err)
	}
	if rec.Version != aiEditUndoVersion || rec.Path == "" {
		return nil, fmt.Errorf("撤销记录格式不支持")
	}
	return &rec, nil
}

func removeAiEditUndo(dataDir, convID string) {
	_ = os.Remove(aiEditUndoPath(dataDir, convID))
}

type aiEditUndoResponse struct {
	Path       string `json:"path"`
	Restored   bool   `json:"restored"`
	NewContent string `json:"new_content"`
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
	rec, err := loadAiEditUndo(s.dataDir, c.ID)
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
	removeAiEditUndo(s.dataDir, c.ID)

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
	})
}
