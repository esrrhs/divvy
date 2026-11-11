package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/tools"
)

// Multi-file turn rollback is journaled and compensated, not instant-
// atomic across files. Each file is replaced with a same-directory temp
// + rename (or a single delete). If a later file fails, already-written
// files are restored from backups. If that compensation also fails, the
// journal and backups are kept and later unsafe writes are blocked until
// recovery succeeds or a conflicting external edit is left untouched.

const (
	rollbackJournalVersion    = 1
	rollbackPhasePreparing    = "preparing"
	rollbackPhaseApplying     = "applying"
	rollbackPhaseCompensating = "compensating"
	rollbackPhaseFailed       = "failed"
	rollbackPhaseDone         = "done"
)

// Deterministic test hooks. Tests must clear them (t.Cleanup).
var (
	rollbackApplyHook         func(path string) error
	rollbackCompensateHook    func(path string) error
	rollbackBeforePrepareHook func()
)

type rollbackJournal struct {
	Version   int                 `json:"version"`
	ID        string              `json:"id"`
	ConvID    string              `json:"conv_id"`
	SessionID string              `json:"session_id"`
	WorkDir   string              `json:"workdir"`
	Phase     string              `json:"phase"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Error     string              `json:"error,omitempty"`
	Conflicts []string            `json:"conflicts,omitempty"`
	Ops       []rollbackJournalOp `json:"ops"`
}

type rollbackJournalOp struct {
	Path    string `json:"path"`
	Delete  bool   `json:"delete,omitempty"`
	Restore string `json:"restore,omitempty"`
	// BeforeHash/TargetHash capture existence and content only. Known
	// modes are compared separately via BeforeMode/RestoreMode; a zero
	// mode is unknown (legacy or the 0000 sentinel) and is not used.
	BeforeHash  string `json:"before_hash"`
	TargetHash  string `json:"target_hash"`
	BackupRel   string `json:"backup_rel,omitempty"`
	Missing     bool   `json:"missing,omitempty"`
	Applied     bool   `json:"applied,omitempty"`
	Compensated bool   `json:"compensated,omitempty"`
	// BeforeMode is the permission bits of the workdir file when it was
	// backed up (the pre-rollback / post-turn file). Zero means unknown.
	BeforeMode uint32 `json:"before_mode,omitempty"`
	// RestoreMode is the pre-turn permission to apply when recreating a
	// file (typically a deleted path). Zero means the snapshot had none.
	RestoreMode uint32 `json:"restore_mode,omitempty"`
}

type pendingRollbackView struct {
	SessionID     string   `json:"session_id"`
	Phase         string   `json:"phase"`
	Error         string   `json:"error,omitempty"`
	Files         []string `json:"files,omitempty"`
	Applied       []string `json:"applied,omitempty"`
	ConflictPaths []string `json:"conflict_paths,omitempty"`
	Message       string   `json:"message"`
}

type rollbackOutcome struct {
	Restored    []string
	Compensated bool
	Pending     bool
	Completed   bool
	Conflicts   []string
	Message     string
	Journal     *rollbackJournal
}

func rollbackJournalRoot(dataDir, convID string) string {
	return filepath.Join(dataDir, "conversations", "rollback", convID)
}

func rollbackJournalPath(dataDir, convID string) string {
	return filepath.Join(rollbackJournalRoot(dataDir, convID), "journal.json")
}

func rollbackBackupDir(dataDir, convID string) string {
	return filepath.Join(rollbackJournalRoot(dataDir, convID), "backups")
}

func journalIsPending(j *rollbackJournal) bool {
	if j == nil {
		return false
	}
	if j.Phase == rollbackPhaseDone {
		return false
	}
	return true
}

func rollbackPendingMessage(j *rollbackJournal) string {
	if j == nil {
		return ""
	}
	base := "上次撤销未完成，工作区可能处于部分恢复状态。已保留操作日志和备份；在恢复完成前会阻止新的写入，以免覆盖未完成的恢复。不会执行 git reset。"
	if strings.TrimSpace(j.Error) != "" {
		return base + " " + strings.TrimSpace(j.Error)
	}
	return base
}

func pendingRollbackFromJournal(j *rollbackJournal) *pendingRollbackView {
	if j == nil || (j.Phase == rollbackPhaseDone) {
		return nil
	}
	v := &pendingRollbackView{
		SessionID:     j.SessionID,
		Phase:         j.Phase,
		Error:         j.Error,
		ConflictPaths: append([]string{}, j.Conflicts...),
		Message:       rollbackPendingMessage(j),
	}
	for _, op := range j.Ops {
		v.Files = append(v.Files, op.Path)
		if op.Applied && !op.Compensated {
			v.Applied = append(v.Applied, op.Path)
		}
	}
	return v
}

func loadRollbackJournal(dataDir, convID string) (*rollbackJournal, error) {
	if !validConversationID(convID) {
		return nil, fmt.Errorf("invalid conversation id")
	}
	raw, err := os.ReadFile(rollbackJournalPath(dataDir, convID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var j rollbackJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("parse rollback journal: %w", err)
	}
	if j.ConvID != "" && j.ConvID != convID {
		return nil, fmt.Errorf("rollback journal conversation id mismatch")
	}
	j.ConvID = convID
	if err := validateRollbackJournal(&j); err != nil {
		return nil, err
	}
	return &j, nil
}

func validateRollbackJournal(j *rollbackJournal) error {
	if j == nil {
		return fmt.Errorf("missing rollback journal")
	}
	if j.Version != rollbackJournalVersion {
		return fmt.Errorf("unsupported rollback journal version %d", j.Version)
	}
	if strings.TrimSpace(j.WorkDir) == "" {
		return fmt.Errorf("rollback journal missing workdir")
	}
	if strings.TrimSpace(j.SessionID) == "" {
		return fmt.Errorf("rollback journal missing session id")
	}
	switch j.Phase {
	case rollbackPhasePreparing, rollbackPhaseApplying, rollbackPhaseCompensating, rollbackPhaseFailed, rollbackPhaseDone:
	default:
		return fmt.Errorf("rollback journal has unknown phase %q", j.Phase)
	}
	return nil
}

func workdirFilePermBits(workdir, rel string) (uint32, error) {
	abs, _, err := resolveProjectPath(workdir, rel)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	return uint32(info.Mode().Perm()), nil
}

func saveRollbackJournal(dataDir string, j *rollbackJournal) error {
	if j == nil || !validConversationID(j.ConvID) {
		return fmt.Errorf("invalid rollback journal")
	}
	j.UpdatedAt = time.Now().UTC()
	dir := rollbackJournalRoot(dataDir, j.ConvID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "journal_*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, rollbackJournalPath(dataDir, j.ConvID))
}

func removeRollbackJournal(dataDir, convID string) error {
	if !validConversationID(convID) {
		return fmt.Errorf("invalid conversation id")
	}
	err := os.RemoveAll(rollbackJournalRoot(dataDir, convID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func journalUnreadableMessage(detail string) string {
	msg := "撤销操作日志无法读取（读取或解析失败，不是文件不存在）。已阻止新的写入，以免覆盖可能未完成的恢复。请检查操作日志后再试恢复。"
	detail = strings.TrimSpace(detail)
	if detail != "" {
		return msg + " " + detail
	}
	return msg
}

// inspectRollbackJournals distinguishes a missing rollback root from a
// read failure, and reports journal dirs that exist but cannot be parsed.
func inspectRollbackJournals(dataDir string) (journals []*rollbackJournal, unreadables []string, err error) {
	root := filepath.Join(dataDir, "conversations", "rollback")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !validConversationID(name) {
			unreadables = append(unreadables, name)
			continue
		}
		j, lerr := loadRollbackJournal(dataDir, name)
		if lerr != nil {
			unreadables = append(unreadables, name)
			continue
		}
		if j != nil {
			journals = append(journals, j)
			continue
		}
		// Directory exists but journal.json does not: leftover empty dirs
		// are ignored; a dir with backups or other files is incomplete.
		dir := rollbackJournalRoot(dataDir, name)
		extras, derr := os.ReadDir(dir)
		if derr != nil {
			unreadables = append(unreadables, name)
			continue
		}
		if len(extras) > 0 {
			unreadables = append(unreadables, name)
		}
	}
	return journals, unreadables, nil
}

func fileDigest(exists bool, content string) string {
	if !exists {
		return "missing"
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func workdirFileState(workdir, rel string) (content string, exists bool, digest string, err error) {
	content, exists, digest, _, err = workdirFileStateMode(workdir, rel)
	return content, exists, digest, err
}

func workdirFileStateMode(workdir, rel string) (content string, exists bool, digest string, mode uint32, err error) {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return "", false, "", 0, err
	}
	cur, err := sb.ReadFile(rel)
	if err != nil {
		if isNotExistish(err) {
			return "", false, "missing", 0, nil
		}
		return "", false, "", 0, err
	}
	bits, perr := workdirFilePermBits(workdir, rel)
	if perr != nil {
		return "", false, "", 0, perr
	}
	return cur, true, fileDigest(true, cur), bits, nil
}

// journalKnownMode reports whether a recorded permission can be compared.
// Zero is the legacy/unknown sentinel; a real Unix 0000 is not distinguished
// and is not treated as a recoverable known mode.
func journalKnownMode(mode uint32) bool {
	return mode != 0
}

func journalOpMatchesBefore(op rollbackJournalOp, exists bool, digest string, mode uint32) bool {
	if digest != op.BeforeHash {
		return false
	}
	if op.BeforeHash == "missing" || !exists {
		return true
	}
	if !journalKnownMode(op.BeforeMode) {
		return true
	}
	return mode == op.BeforeMode
}

func journalOpMatchesTarget(op rollbackJournalOp, exists bool, digest string, mode uint32) bool {
	if digest != op.TargetHash {
		return false
	}
	if op.TargetHash == "missing" || !exists {
		return true
	}
	if !journalKnownMode(op.RestoreMode) {
		return true
	}
	return mode == op.RestoreMode
}

func targetDigest(op rollbackOp) string {
	if op.delete {
		return "missing"
	}
	return fileDigest(true, op.restore)
}

func newRollbackJournalID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rbj_" + hex.EncodeToString(b[:]), nil
}

func backupRelFor(path string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(path))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid backup path %s", path)
	}
	return rel, nil
}

func copyWorkdirBackup(workdir, rel, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	abs, _, err := resolveProjectPath(workdir, rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".bak-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, dest)
}

func atomicWriteWorkdirFile(workdir, rel, content string) error {
	return atomicWriteWorkdirFileMode(workdir, rel, content, 0)
}

// atomicWriteWorkdirFileMode writes one project file via temp+rename and
// applies perm before the rename. A zero perm keeps the existing file's
// mode when overwriting, or 0644 when creating a new path. This is the
// per-file atomic unit; it does not make a multi-file rollback instant-atomic.
func atomicWriteWorkdirFileMode(workdir, rel, content string, perm os.FileMode) error {
	abs, _, err := resolveProjectPath(workdir, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	if perm == 0 {
		if info, statErr := os.Stat(abs); statErr == nil {
			perm = info.Mode().Perm()
		} else {
			perm = 0644
		}
	} else {
		perm = perm.Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".divvy-rb-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(content)); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, abs)
}

func applyRollbackOpAtomic(workdir string, op rollbackOp) error {
	if op.delete {
		return writeTurnResultDelete(workdir, op.path, "", true)
	}
	return atomicWriteWorkdirFileMode(workdir, op.path, op.restore, op.restoreMode)
}

func prepareRollbackJournal(dataDir, convID, sessionID, workdir string, plan []rollbackOp) (*rollbackJournal, error) {
	id, err := newRollbackJournalID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	j := &rollbackJournal{
		Version:   rollbackJournalVersion,
		ID:        id,
		ConvID:    convID,
		SessionID: sessionID,
		WorkDir:   workdir,
		Phase:     rollbackPhasePreparing,
		CreatedAt: now,
		UpdatedAt: now,
	}
	bakRoot := rollbackBackupDir(dataDir, convID)
	if err := os.MkdirAll(bakRoot, 0755); err != nil {
		return nil, err
	}
	for _, op := range plan {
		cur, exists, digest, err := workdirFileState(workdir, op.path)
		_ = cur
		if err != nil {
			_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
			return nil, err
		}
		if op.beforeHash != "" && digest != op.beforeHash {
			_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
			return nil, fmt.Errorf("工作区中的 %s 在撤销开始前已被改动，已停止，不会覆盖这次外部修改，也未建立新的备份基线", op.path)
		}
		jop := rollbackJournalOp{
			Path:        op.path,
			Delete:      op.delete,
			Restore:     op.restore,
			BeforeHash:  digest,
			TargetHash:  targetDigest(op),
			Missing:     !exists,
			RestoreMode: uint32(op.restoreMode.Perm()),
		}
		if op.restoreMode == 0 {
			jop.RestoreMode = 0
		}
		if exists {
			rel, berr := backupRelFor(op.path)
			if berr != nil {
				_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
				return nil, berr
			}
			jop.BackupRel = rel
			abs, _, rerr := resolveProjectPath(workdir, op.path)
			if rerr == nil {
				if info, sterr := os.Stat(abs); sterr == nil {
					jop.BeforeMode = uint32(info.Mode().Perm())
				}
			}
			if err := copyWorkdirBackup(workdir, op.path, filepath.Join(bakRoot, rel)); err != nil {
				_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
				return nil, fmt.Errorf("backup %s: %w", op.path, err)
			}
		}
		j.Ops = append(j.Ops, jop)
	}
	if err := saveRollbackJournal(dataDir, j); err != nil {
		_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
		return nil, err
	}
	j.Phase = rollbackPhaseApplying
	if err := saveRollbackJournal(dataDir, j); err != nil {
		_ = os.RemoveAll(rollbackJournalRoot(dataDir, convID))
		return nil, err
	}
	return j, nil
}

func journalOpToRollbackOp(op rollbackJournalOp) rollbackOp {
	return rollbackOp{path: op.Path, restore: op.Restore, delete: op.Delete, restoreMode: os.FileMode(op.RestoreMode)}
}

func (s *Server) applyJournaledRollback(c *Conversation, sid string, plan []rollbackOp) (*rollbackOutcome, error) {
	if hook := rollbackBeforePrepareHook; hook != nil {
		hook()
	}
	j, err := prepareRollbackJournal(s.dataDir, c.ID, sid, c.WorkDir, plan)
	if err != nil {
		return &rollbackOutcome{Message: err.Error()}, err
	}
	return s.runRollbackApply(c, j, plan)
}

func (s *Server) runRollbackApply(c *Conversation, j *rollbackJournal, plan []rollbackOp) (*rollbackOutcome, error) {
	byPath := map[string]rollbackOp{}
	for _, op := range plan {
		byPath[op.path] = op
	}
	var applied []string
	for i := range j.Ops {
		op := journalOpToRollbackOp(j.Ops[i])
		if planned, ok := byPath[op.path]; ok {
			op = planned
		}
		if hook := rollbackApplyHook; hook != nil {
			if herr := hook(op.path); herr != nil {
				return s.compensateRollback(c, j, applied, herr.Error(), nil)
			}
		}
		_, exists, digest, mode, err := workdirFileStateMode(j.WorkDir, op.path)
		if err != nil {
			return s.compensateRollback(c, j, applied, err.Error(), nil)
		}
		if !journalOpMatchesBefore(j.Ops[i], exists, digest, mode) {
			conflicts := []string{op.path}
			return s.compensateRollback(c, j, applied, "工作区中的 "+op.path+" 在撤销写入前已被改动，已停止并尝试恢复已写文件，不会覆盖这次外部修改", conflicts)
		}
		if err := applyRollbackOpAtomic(j.WorkDir, op); err != nil {
			return s.compensateRollback(c, j, applied, err.Error(), nil)
		}
		j.Ops[i].Applied = true
		if err := saveRollbackJournal(s.dataDir, j); err != nil {
			return s.compensateRollback(c, j, append(append([]string{}, applied...), op.path), err.Error(), nil)
		}
		applied = append(applied, op.path)
	}
	j.Phase = rollbackPhaseDone
	j.Error = ""
	j.Conflicts = nil
	if err := saveRollbackJournal(s.dataDir, j); err != nil {
		// Files are already at the restored state. Keep the done journal
		// so a restart can record RolledBackTurns.
		return &rollbackOutcome{Restored: applied, Completed: false, Pending: true, Message: "文件已恢复，但未能写入完成记录：" + err.Error(), Journal: j}, err
	}
	if err := s.commitRolledBackTurn(c, j.SessionID, applied, false); err != nil {
		return &rollbackOutcome{Restored: applied, Completed: false, Pending: true, Message: "文件已恢复，但未能更新对话记录：" + err.Error(), Journal: j}, err
	}
	if err := removeRollbackJournal(s.dataDir, j.ConvID); err != nil {
		// Conversation already matches the files; leftover journal of a
		// completed rollback is cleaned on the next recover.
		return &rollbackOutcome{Restored: applied, Completed: true, Message: "", Journal: nil}, nil
	}
	return &rollbackOutcome{Restored: applied, Completed: true, Journal: nil}, nil
}

func (s *Server) compensateRollback(c *Conversation, j *rollbackJournal, applied []string, reason string, conflicts []string) (*rollbackOutcome, error) {
	j.Phase = rollbackPhaseCompensating
	setJournalConflicts(j, conflicts, reason)
	if err := saveRollbackJournal(s.dataDir, j); err != nil {
		j.Phase = rollbackPhaseFailed
		return &rollbackOutcome{Pending: true, Conflicts: j.Conflicts, Message: rollbackPendingMessage(j), Journal: j}, fmt.Errorf("%s；并且无法更新操作日志：%w", reason, err)
	}
	fail, left, newConflicts, err := s.compensateAppliedOps(j)
	base := reason
	if err != nil {
		if base == "" {
			base = err.Error()
		} else if !strings.Contains(base, err.Error()) {
			base = strings.TrimSpace(base + "；补偿失败：" + err.Error())
		}
	}
	setJournalConflicts(j, newConflicts, base)
	if fail || err != nil || len(j.Conflicts) > 0 {
		j.Phase = rollbackPhaseFailed
		_ = saveRollbackJournal(s.dataDir, j)
		s.noteRollbackPending(c, j)
		msg := rollbackPendingMessage(j)
		return &rollbackOutcome{Pending: true, Compensated: false, Conflicts: j.Conflicts, Message: msg, Journal: j}, fmt.Errorf("%s", msg)
	}
	// Fully returned to the pre-rollback state. Do not mark the turn
	// rolled back; drop the journal so a later retry can start clean.
	_ = applied
	_ = left
	if err := removeRollbackJournal(s.dataDir, j.ConvID); err != nil {
		j.Phase = rollbackPhaseFailed
		j.Error = "已把已写文件恢复到撤销前，但未能清除操作日志：" + err.Error()
		_ = saveRollbackJournal(s.dataDir, j)
		s.noteRollbackPending(c, j)
		return &rollbackOutcome{Pending: true, Compensated: true, Message: rollbackPendingMessage(j), Journal: j}, fmt.Errorf("%s", rollbackPendingMessage(j))
	}
	s.noteRollbackCompensated(c, reason)
	return &rollbackOutcome{
		Compensated: true,
		Pending:     false,
		Message:     "撤销进行到中途失败，已将已改动的文件恢复到撤销前的内容。对话未标记为已撤销。请检查后重试。原因：" + reason,
	}, fmt.Errorf("撤销进行到中途失败，已将已改动的文件恢复到撤销前的内容。原因：%s", reason)
}

func (s *Server) compensateAppliedOps(j *rollbackJournal) (failed bool, leftover []string, conflicts []string, err error) {
	bakRoot := rollbackBackupDir(s.dataDir, j.ConvID)
	for i := len(j.Ops) - 1; i >= 0; i-- {
		op := &j.Ops[i]
		if op.Compensated {
			continue
		}
		_, exists, digest, mode, rerr := workdirFileStateMode(j.WorkDir, op.Path)
		if rerr != nil {
			return true, leftoverPaths(j), conflicts, rerr
		}
		if journalOpMatchesBefore(*op, exists, digest, mode) {
			if op.Applied {
				op.Applied = false
				op.Compensated = true
				if serr := saveRollbackJournal(s.dataDir, j); serr != nil {
					return true, leftoverPaths(j), conflicts, serr
				}
			}
			continue
		}
		// A file already at the rollback target (content and, when
		// recorded, mode) was written even if Applied never landed.
		atTarget := journalOpMatchesTarget(*op, exists, digest, mode)
		wrote := op.Applied || atTarget
		if !wrote {
			continue
		}
		if !atTarget {
			conflicts = append(conflicts, op.Path)
			continue
		}
		if hook := rollbackCompensateHook; hook != nil {
			if herr := hook(op.Path); herr != nil {
				return true, leftoverPaths(j), conflicts, herr
			}
		}
		var cerr error
		if op.Missing {
			cerr = writeTurnResultDelete(j.WorkDir, op.Path, "", true)
		} else {
			raw, berr := os.ReadFile(filepath.Join(bakRoot, op.BackupRel))
			if berr != nil {
				return true, leftoverPaths(j), conflicts, fmt.Errorf("read backup %s: %w", op.Path, berr)
			}
			cerr = atomicWriteWorkdirFileMode(j.WorkDir, op.Path, string(raw), os.FileMode(op.BeforeMode))
		}
		if cerr != nil {
			return true, leftoverPaths(j), conflicts, cerr
		}
		op.Applied = false
		op.Compensated = true
		if serr := saveRollbackJournal(s.dataDir, j); serr != nil {
			return true, leftoverPaths(j), conflicts, serr
		}
	}
	return false, leftoverPaths(j), conflicts, nil
}

func leftoverPaths(j *rollbackJournal) []string {
	var out []string
	for _, op := range j.Ops {
		if op.Compensated {
			continue
		}
		if op.Applied {
			out = append(out, op.Path)
			continue
		}
		if j == nil || strings.TrimSpace(j.WorkDir) == "" {
			continue
		}
		_, exists, digest, mode, err := workdirFileStateMode(j.WorkDir, op.Path)
		if err == nil && journalOpMatchesTarget(op, exists, digest, mode) && !journalOpMatchesBefore(op, exists, digest, mode) {
			out = append(out, op.Path)
		}
	}
	return out
}

func setJournalConflicts(j *rollbackJournal, conflicts []string, base string) {
	if j == nil {
		return
	}
	j.Conflicts = uniqueStrings(conflicts)
	err := strings.TrimSpace(base)
	if err == "" {
		err = strings.TrimSpace(j.Error)
	}
	if i := strings.Index(err, " 检测到外部修改"); i >= 0 {
		err = strings.TrimSpace(err[:i])
	}
	if len(j.Conflicts) > 0 {
		err = strings.TrimSpace(err + " 检测到外部修改（" + strings.Join(j.Conflicts, ", ") + "），未覆盖这些文件。")
	}
	j.Error = err
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (s *Server) commitRolledBackTurn(c *Conversation, sid string, done []string, alreadyNoted bool) error {
	if s == nil || c == nil {
		return fmt.Errorf("missing conversation")
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, lerr := loadConversation(s.dataDir, c.ID)
	if lerr == nil {
		*c = *latest
	}
	if !containsString(c.RolledBackTurns, sid) {
		c.RolledBackTurns = append(append([]string{}, c.RolledBackTurns...), sid)
	}
	if !alreadyNoted {
		notice := "已撤销这一轮对工作区的修改，只动了该轮涉及的文件（" + strings.Join(done, ", ") + "）。这些改动不再存在于磁盘。之后的回复不要假设它们仍然有效。按块接受/拒绝是写回当前工作区的手工挑选；这次是把整轮结果恢复到该轮开始前。较早轮次的快照仍可只读回看。多文件撤销按文件原子替换并有操作日志；不是多文件瞬间同时提交。"
		c.appendMessage("system", kindNotice, notice, sid, "turn_rolled_back", secrets)
		c.appendWorkLog("撤销轮次 "+sid+" 的文件："+strings.Join(done, ", "), secrets)
	}
	if err := markTurnChangesRolledBack(s.dataDir, c.ID, sid, secrets); err != nil {
		return err
	}
	if err := saveConversation(s.dataDir, c, secrets); err != nil {
		return err
	}
	return nil
}

func markTurnChangesRolledBack(dataDir, convID, sid string, secrets []string) error {
	changes, err := loadTurnChanges(dataDir, convID, sid)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	changed := false
	for i := range changes {
		if changes[i].AppliedStatus == "rolled_back" {
			continue
		}
		changes[i].AppliedStatus = "rolled_back"
		if pre, existed, ok := preTurnContent(changes[i]); ok {
			if existed {
				changes[i].Applied = pre
			} else {
				changes[i].Applied = ""
			}
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return saveTurnChanges(dataDir, convID, sid, changes, secrets)
}

func (s *Server) noteRollbackPending(c *Conversation, j *rollbackJournal) {
	if s == nil || c == nil || j == nil {
		return
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := loadConversation(s.dataDir, c.ID)
	if err == nil {
		*c = *latest
	}
	text := rollbackPendingMessage(j)
	if lastRollbackNoticeIs(c, text) {
		return
	}
	c.appendMessage("system", kindNotice, text, j.SessionID, "rollback_pending", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
}

func (s *Server) noteRollbackCompensated(c *Conversation, reason string) {
	if s == nil || c == nil {
		return
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := loadConversation(s.dataDir, c.ID)
	if err == nil {
		*c = *latest
	}
	text := "撤销进行到中途失败，已将已改动的文件恢复到撤销前的内容。这一轮未标记为已撤销。原因：" + strings.TrimSpace(reason)
	c.appendMessage("system", kindNotice, text, c.CurrentSessionID, "rollback_compensated", secrets)
	_ = saveConversation(s.dataDir, c, secrets)
}

func lastRollbackNoticeIs(c *Conversation, text string) bool {
	if c == nil || len(c.Messages) == 0 {
		return false
	}
	last := c.Messages[len(c.Messages)-1]
	return last.Kind == kindNotice && (last.Outcome == "rollback_pending" || strings.Contains(last.Text, "上次撤销未完成")) && last.Text == text
}

// recoverRollbackJournal resumes a crash or failed mid-rollback. If every
// planned file already matches the restore target (content and, when
// recorded, mode), the turn is marked rolled back. Otherwise applied
// files are compensated from backups, except paths whose current
// content or known mode no longer matches the rollback target (those
// are treated as external edits and left alone).
func (s *Server) recoverRollbackJournal(j *rollbackJournal, takeLease bool) (*rollbackOutcome, error) {
	if s == nil || j == nil {
		return nil, fmt.Errorf("missing rollback journal")
	}
	if takeLease {
		unlock, err := s.mgr.BeginWorkdirWriteExclusive(j.WorkDir, "rollback-recover:"+j.ConvID)
		if err != nil {
			return &rollbackOutcome{Pending: true, Journal: j, Message: "工作区正被占用，无法恢复未完成的撤销：" + err.Error()}, err
		}
		defer unlock()
	}
	c, _ := loadConversation(s.dataDir, j.ConvID)
	if c == nil {
		c = &Conversation{ID: j.ConvID, WorkDir: j.WorkDir}
	}

	if j.Phase == rollbackPhaseDone || journalAllRestored(j) {
		if !containsString(c.RolledBackTurns, j.SessionID) {
			if err := s.commitRolledBackTurn(c, j.SessionID, journalPaths(j), false); err != nil {
				return &rollbackOutcome{Pending: true, Restored: journalPaths(j), Journal: j, Message: "文件已是撤销后的状态，但未能更新对话记录：" + err.Error()}, err
			}
		}
		_ = removeRollbackJournal(s.dataDir, j.ConvID)
		return &rollbackOutcome{Completed: true, Restored: journalPaths(j)}, nil
	}

	// Partial or failed: restore pre-rollback state when safe.
	fail, leftover, conflicts, err := s.compensateAppliedOps(j)
	base := ""
	if err != nil {
		base = err.Error()
	}
	setJournalConflicts(j, conflicts, base)
	if fail || err != nil || len(leftover) > 0 || len(j.Conflicts) > 0 {
		j.Phase = rollbackPhaseFailed
		_ = saveRollbackJournal(s.dataDir, j)
		s.noteRollbackPending(c, j)
		return &rollbackOutcome{Pending: true, Conflicts: j.Conflicts, Message: rollbackPendingMessage(j), Journal: j}, fmt.Errorf("%s", rollbackPendingMessage(j))
	}
	if err := removeRollbackJournal(s.dataDir, j.ConvID); err != nil {
		j.Phase = rollbackPhaseFailed
		j.Error = "已恢复到撤销前，但未能清除操作日志：" + err.Error()
		_ = saveRollbackJournal(s.dataDir, j)
		return &rollbackOutcome{Pending: true, Compensated: true, Message: rollbackPendingMessage(j), Journal: j}, err
	}
	s.noteRollbackCompensated(c, "进程重启或再次恢复时完成了补偿")
	return &rollbackOutcome{Compensated: true, Message: "未完成的撤销已恢复到撤销前的文件状态，未标记为已撤销。"}, nil
}

func journalAllRestored(j *rollbackJournal) bool {
	if j == nil || len(j.Ops) == 0 {
		return false
	}
	for _, op := range j.Ops {
		_, exists, digest, mode, err := workdirFileStateMode(j.WorkDir, op.Path)
		if err != nil || !journalOpMatchesTarget(op, exists, digest, mode) {
			return false
		}
	}
	return true
}

func journalPaths(j *rollbackJournal) []string {
	var out []string
	for _, op := range j.Ops {
		out = append(out, op.Path)
	}
	return out
}

func (s *Server) rollbackPendingBlock(c *Conversation) (string, bool) {
	if s == nil || c == nil {
		return "", false
	}
	return s.rollbackPendingBlockByID(c.ID, c)
}

// rollbackWriteBlock is the recovery barrier for normal writes and new
// runs: the conversation's own journal, and any pending journal for the
// same workdir (another conversation or a leftover after restart).
func (s *Server) rollbackWriteBlock(c *Conversation) (string, bool) {
	if reason, blocked := s.rollbackPendingBlock(c); blocked {
		return reason, true
	}
	if c == nil {
		return "", false
	}
	return s.rollbackPendingForWorkdir(c.WorkDir)
}

func (s *Server) rollbackPendingBlockByID(convID string, c *Conversation) (string, bool) {
	if s == nil || convID == "" {
		return "", false
	}
	j, err := loadRollbackJournal(s.dataDir, convID)
	if err != nil {
		return journalUnreadableMessage(err.Error()), true
	}
	if j == nil {
		return "", false
	}
	if j.Phase == rollbackPhaseDone && c != nil && containsString(c.RolledBackTurns, j.SessionID) {
		return "", false
	}
	if j.Phase == rollbackPhaseDone {
		return rollbackPendingMessage(&rollbackJournal{Error: "文件已恢复，对话记录尚未写完。请先完成恢复。", Phase: j.Phase, SessionID: j.SessionID}), true
	}
	return rollbackPendingMessage(j), true
}

func (s *Server) rollbackPendingForWorkdir(workdir string) (string, bool) {
	if s == nil || strings.TrimSpace(workdir) == "" {
		return "", false
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	list, unreadables, err := inspectRollbackJournals(s.dataDir)
	if err != nil {
		return journalUnreadableMessage(err.Error()), true
	}
	if len(unreadables) > 0 {
		return journalUnreadableMessage("受影响的记录：" + strings.Join(unreadables, ", ")), true
	}
	for _, j := range list {
		if j == nil || !journalIsPending(j) && j.Phase != rollbackPhaseDone {
			continue
		}
		jw, jerr := filepath.Abs(j.WorkDir)
		if jerr != nil {
			jw = j.WorkDir
		}
		if filepath.Clean(jw) != filepath.Clean(abs) {
			continue
		}
		if j.Phase == rollbackPhaseDone {
			if c, lerr := loadConversation(s.dataDir, j.ConvID); lerr == nil && containsString(c.RolledBackTurns, j.SessionID) {
				continue
			}
		}
		return rollbackPendingMessage(j), true
	}
	return "", false
}

func (s *Server) recoverAllRollbackJournals() {
	if s == nil {
		return
	}
	list, _, err := inspectRollbackJournals(s.dataDir)
	if err != nil {
		return
	}
	for _, j := range list {
		_, _ = s.recoverRollbackJournal(j, true)
	}
}
