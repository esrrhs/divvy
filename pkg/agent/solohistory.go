package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/esrrhs/divvy/pkg/llm"
)

// Solo conversation persistence.
//
// Tree mode has somewhere to resume to: the nodes and their states are the
// state. Solo's state IS the conversation — which files it wrote, why it
// changed them, what it already tried — and none of that is in the tree. So a
// fresh RunSolo after a crash used to start from a populated workspace with no
// memory of building it, which a measured run answered by rewriting every file
// from scratch and discarding work that already passed its tests.
//
// The transcript is written as JSONL (one message per line) so a crash mid-write
// costs at most the last line, and reading back tolerates it.

// loadSoloHistory reads a persisted transcript. A missing file is not an error
// (first run); a truncated tail is dropped rather than failing the whole load.
func loadSoloHistory(path string) ([]llm.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var msgs []llm.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m llm.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// A torn final line is the expected crash signature. Anything
			// earlier means the file is not ours; stop and keep the prefix.
			break
		}
		if m.Role == "" {
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return msgs, err
	}
	return msgs, nil
}

// saveSoloHistory rewrites the transcript atomically: a reader (or a later
// resume) sees either the previous complete file or the new one.
func saveSoloHistory(path string, msgs []llm.Message) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	var b strings.Builder
	for _, m := range msgs {
		line, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, "solo_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
