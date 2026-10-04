package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
)

// Storage handles persisting and reloading TaskTree states.
type Storage struct {
	baseDir string
}

// NewStorage initializes a storage instance with a target base directory.
func NewStorage(baseDir string) (*Storage, error) {
	if baseDir == "" {
		baseDir = ".divvy"
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory %s: %w", baseDir, err)
	}
	return &Storage{baseDir: baseDir}, nil
}

// GetTreeFilePath returns standard file path for a tree session.
func (s *Storage) GetTreeFilePath(sessionID string) string {
	return filepath.Join(s.baseDir, fmt.Sprintf("%s.json", sessionID))
}

// SaveTree saves the task tree atomically to avoid corruption on crash.
func (s *Storage) SaveTree(tree *TaskTree) error {
	if tree == nil {
		return fmt.Errorf("cannot save nil tree")
	}

	data, err := tree.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize tree: %w", err)
	}

	targetPath := s.GetTreeFilePath(tree.ID)
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory %s: %w", dir, err)
	}

	// Write to temporary file first
	tmpFile, err := os.CreateTemp(dir, fmt.Sprintf("tree_%s_*.tmp", tree.ID))
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write tree data to temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomically replace the destination file
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically rename temp file to %s: %w", targetPath, err)
	}

	return nil
}

// LoadTree loads a task tree from the storage by session ID.
func (s *Storage) LoadTree(sessionID string) (*TaskTree, error) {
	targetPath := s.GetTreeFilePath(sessionID)
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read tree file %s: %w", targetPath, err)
	}

	tree, err := FromJSON(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tree JSON from %s: %w", targetPath, err)
	}

	return tree, nil
}

// SessionArtifacts lists every file on disk that belongs to one session: the
// task tree plus its run log and JSONL event stream, when they exist.
type SessionArtifacts struct {
	ID    string
	Paths []string
	Bytes int64
}

// PruneResult reports what a prune removed (or would remove under a dry run).
type PruneResult struct {
	Removed []SessionArtifacts // oldest first
	Kept    []string           // session ids left on disk
	Bytes   int64
}

// PlanPrune chooses which sessions a prune with the given keep count would
// delete: the oldest ones beyond `keep`, newest-first ordering. The session
// the LATEST pointer names is exempt, so a prune can never orphan the session
// a plain `-resume` would pick up.
func (s *Storage) PlanPrune(keep int) (*PruneResult, error) {
	if keep < 1 {
		return nil, fmt.Errorf("keep must be >= 1 (got %d): pruning everything would delete the session you are working on", keep)
	}
	sessions, err := s.ListSessions()
	if err != nil {
		return nil, err
	}
	latest := s.latestID()
	res := &PruneResult{}
	for i, info := range sessions {
		if i < keep || info.ID == latest {
			res.Kept = append(res.Kept, info.ID)
			continue
		}
		art := SessionArtifacts{ID: info.ID, Paths: s.existingArtifacts(info.ID)}
		for _, p := range art.Paths {
			if st, serr := os.Stat(p); serr == nil {
				art.Bytes += st.Size()
			}
		}
		res.Removed = append(res.Removed, art)
		res.Bytes += art.Bytes
	}
	return res, nil
}

// PruneSessions deletes the sessions chosen by PlanPrune, along with their
// run log and event stream. Unreadable files are skipped and reported rather
// than aborting the whole prune.
func (s *Storage) PruneSessions(keep int) (*PruneResult, error) {
	plan, err := s.PlanPrune(keep)
	if err != nil {
		return nil, err
	}
	for i := range plan.Removed {
		var kept []string
		for _, p := range plan.Removed[i].Paths {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				kept = append(kept, p)
			}
		}
		plan.Removed[i].Paths = kept
	}
	return plan, nil
}

// existingArtifacts returns the paths of the files that exist for a session.
func (s *Storage) existingArtifacts(id string) []string {
	candidates := []string{
		s.GetTreeFilePath(id),
		filepath.Join(s.baseDir, "logs", id+".log"),
		filepath.Join(s.baseDir, "events", id+".jsonl"),
	}
	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// latestID reads the LATEST pointer, returning "" when there is none.
func (s *Storage) latestID() string {
	data, err := os.ReadFile(filepath.Join(s.baseDir, "LATEST"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// TreeExists checks whether a persisted session tree exists.
func (s *Storage) TreeExists(sessionID string) bool {
	targetPath := s.GetTreeFilePath(sessionID)
	_, err := os.Stat(targetPath)
	return err == nil
}

// SessionInfo is a lightweight summary of one saved session.
type SessionInfo struct {
	ID         string
	Goal       string
	UpdatedAt  time.Time
	RootState  models.TaskState
	LeavesDone int
	LeavesAll  int
}

// ListSessions scans the storage directory for saved trees, newest first.
// Unreadable or non-tree JSON files are skipped.
func (s *Storage) ListSessions() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		tree, err := s.LoadTree(id)
		if err != nil {
			continue
		}
		done, total, _ := tree.GetLeafProgress()
		info := SessionInfo{
			ID:         id,
			Goal:       tree.Goal,
			UpdatedAt:  tree.UpdatedAt,
			LeavesDone: done,
			LeavesAll:  total,
		}
		if root, ok := tree.GetNode(tree.RootID); ok {
			info.RootState = root.State
			// Trees saved outside the orchestrator may have an empty Goal;
			// the root title/description still identifies the session.
			if info.Goal == "" {
				info.Goal = root.Title
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
