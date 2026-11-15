package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
// aiEditUndoFile is one file inside a single undo step. A multi-file rewrite
// is ONE undo step on purpose: Cmd-Z after "rename X and update its callers"
// must put all of them back, not just the first one.
type aiEditUndoFile struct {
	Path       string `json:"path"`
	Restore    string `json:"restore"`
	BeforeHash string `json:"before_hash"`
	TargetHash string `json:"target_hash"`
}

type aiEditUndoRecord struct {
	Version int    `json:"version"`
	ConvID  string `json:"conv_id"`
	WorkDir string `json:"workdir"`
	// Path/Restore/BeforeHash/TargetHash describe the first file and are kept
	// for records written before multi-file support.
	Path       string `json:"path"`
	Restore    string `json:"restore"`
	BeforeHash string `json:"before_hash"`
	TargetHash string `json:"target_hash"`
	// Files is the full set. Empty on old records; normalizeAiEditUndoFiles
	// derives one entry from the legacy fields on read.
	Files []aiEditUndoFile `json:"files,omitempty"`
	At    time.Time        `json:"at"`
}

func normalizeAiEditUndoFiles(rec *aiEditUndoRecord) {
	if rec == nil || len(rec.Files) > 0 || rec.Path == "" {
		return
	}
	rec.Files = []aiEditUndoFile{{
		Path:       rec.Path,
		Restore:    rec.Restore,
		BeforeHash: rec.BeforeHash,
		TargetHash: rec.TargetHash,
	}}
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
		normalizeAiEditUndoFiles(&old)
		return &aiEditUndoStack{Version: aiEditUndoVersion, Records: []*aiEditUndoRecord{&old}}, nil
	}
	var stack aiEditUndoStack
	if err := json.Unmarshal(raw, &stack); err != nil {
		return nil, fmt.Errorf("撤销记录损坏：%w", err)
	}
	if stack.Version != aiEditUndoVersion {
		return nil, fmt.Errorf("撤销记录格式不支持")
	}
	for _, rec := range stack.Records {
		normalizeAiEditUndoFiles(rec)
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
	// Paths are all files restored by this one undo step.
	Paths []string `json:"paths,omitempty"`
}

// handleAiEditUndo reverses the last inline edit step. A multi-file round is
// ONE step: every file is checked before any is restored, so a conflict on
// one file cannot leave the others rolled back on their own.
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
	normalizeAiEditUndoFiles(rec)
	files := rec.Files
	if len(files) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "撤销记录里没有文件，已停止"})
		return
	}

	unlock, lerr := s.mgr.BeginWorkdirWrite(c.WorkDir, "ai-edit-undo:"+c.ID)
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
	// Check ALL files before touching ANY of them. The current contents are
	// kept for compensation: a failed restore must be able to put the files
	// it already changed back exactly as they were.
	current := make([]string, len(files))
	for i, f := range files {
		content, err := sb.ReadFile(f.Path)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":         "文件已不存在或不可读，无法撤销：" + err.Error(),
				"conflict_path": f.Path,
			})
			return
		}
		if fileSHA256([]byte(content)) != f.TargetHash {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":         "文件在这次 AI 改写之后又被修改过，撤销会覆盖那些改动，已停止",
				"conflict_path": f.Path,
			})
			return
		}
		current[i] = content
	}
	var restored []int
	for i, f := range files {
		if err := sb.WriteFile(f.Path, f.Restore); err != nil {
			msg := "恢复 " + f.Path + " 失败：" + err.Error()
			// Put back the files already undone, so the step stays atomic
			// from the user's point of view.
			var first error
			for k := len(restored) - 1; k >= 0; k-- {
				idx := restored[k]
				if e := sb.WriteFile(files[idx].Path, current[idx]); e != nil && first == nil {
					first = e
				}
			}
			if first != nil {
				msg += "；且把已恢复的文件写回去失败：" + first.Error() + "（请手动检查）"
			} else if len(restored) > 0 {
				msg += "；已把本次其它文件写回原状"
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
			return
		}
		restored = append(restored, i)
	}
	// The record is dropped only after every restore actually happened.
	if err := writeAiEditUndoStack(s.dataDir, c.ID, stack); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "文件已恢复，但撤销记录保存失败: " + err.Error()})
		return
	}

	secrets := s.secretsFor(c.ID)
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	s.mu.Lock()
	if latest, lerr := loadConversation(s.dataDir, c.ID); lerr == nil {
		c = latest
	}
	c.appendMessage("system", kindNotice,
		fmt.Sprintf("已撤销上一次 AI 改写，%s 恢复到写回前的内容。", strings.Join(paths, "、")),
		"", "ai_edit_undone", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, aiEditUndoResponse{
		Path:       files[0].Path,
		Paths:      paths,
		Restored:   true,
		NewContent: files[0].Restore,
		Remaining:  len(stack.Records),
	})
}
