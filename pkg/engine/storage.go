package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/models"
)

// Storage handles persisting and reloading TaskTree states.
type Storage struct {
	baseDir string
}

// NewStorage initializes a storage instance with a target base directory.
func NewStorage(baseDir string) (*Storage, error) {
	if baseDir == "" {
		baseDir = ".go_llm_engine"
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
