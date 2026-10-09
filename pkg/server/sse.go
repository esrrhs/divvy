package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

// replayEventCount is how many recent JSONL events are replayed on (re)connect.
// Combined with the fresh tree snapshot it restores the UI state after a
// dropped connection without re-reading an unbounded file.
const replayEventCount = 200

// sseEventMaxLine bounds one JSONL line during replay. Tree snapshots can be
// large, so the scanner gets a generous ceiling rather than bufio's 64 KiB.
const sseEventMaxLine = 16 * 1024 * 1024

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing session id"})
		return
	}

	live := s.mgr.Get(id)
	treePath := filepath.Join(s.dataDir, id+".json")
	if live == nil {
		if _, err := os.Stat(treePath); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var secrets []string
	var eventPath string
	if live != nil {
		eventPath = live.Orchestrator().EventPath()
		secrets = live.SecretValues()
	} else {
		eventPath = filepath.Join(s.dataDir, "events", id+".jsonl")
	}

	// Resume point advertised by a reconnecting EventSource.
	var afterSeq int64
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if v, err := strconv.ParseInt(last, 10, 64); err == nil {
			afterSeq = v
		}
	}

	// Subscribe BEFORE reading the replay: events produced while the file is
	// read land in the buffered channel and are de-duplicated by seq after.
	var liveCh <-chan map[string]any
	var cancelLive func()
	if live != nil {
		liveCh, cancelLive = live.SubscribeEvents()
		defer cancelLive()
	}

	// 1) Replay the JSONL tail (or everything after Last-Event-ID when the
	// client resuming inside the tail window).
	maxSeq := s.replayEvents(w, flusher, eventPath, afterSeq, secrets)

	// 2) One authoritative tree snapshot, so a fresh UI never has to rebuild
	// state from events alone.
	if data, err := s.currentTreeJSON(live, id, treePath); err == nil {
		writeSSEEvent(w, flusher, "", data, secrets)
	}

	// 3) Live fan-out + heartbeat. A read-only view of a finished session
	// keeps the connection open with heartbeats only.
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	rc := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-liveCh:
			if !ok {
				return
			}
			seq, _ := ev["seq"].(int64) // synthetic notes (e.g. _dropped) have none
			if seq > 0 && seq <= maxSeq {
				continue // covered by the replay
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			idStr := ""
			if seq > 0 {
				idStr = strconv.FormatInt(seq, 10)
			}
			writeSSEEvent(w, flusher, idStr, data, secrets)
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// replayEvents streams the last replayEventCount persisted events (optionally
// only those newer than afterSeq) and returns the highest seq sent.
func (s *Server) replayEvents(w http.ResponseWriter, f http.Flusher, path string, afterSeq int64, secrets []string) int64 {
	lines, err := tailLines(path, replayEventCount)
	if err != nil {
		return afterSeq
	}
	maxSeq := afterSeq
	for _, line := range lines {
		var head struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue // tolerate a torn final line
		}
		if head.Seq > 0 && head.Seq <= afterSeq {
			if head.Seq > maxSeq {
				maxSeq = head.Seq
			}
			continue
		}
		idStr := ""
		if head.Seq > 0 {
			idStr = strconv.FormatInt(head.Seq, 10)
			maxSeq = max(maxSeq, head.Seq)
		}
		writeSSEEvent(w, f, idStr, line, secrets)
	}
	f.Flush()
	return maxSeq
}

// currentTreeJSON wraps the live or persisted tree in a tree_snapshot event
// matching the orchestrator's own event shape.
func (s *Server) currentTreeJSON(live *agent.RunHandle, id, treePath string) ([]byte, error) {
	var raw []byte
	var err error
	if live != nil {
		raw, err = live.Orchestrator().Tree().ToJSON()
	} else {
		raw, err = os.ReadFile(treePath)
	}
	if err != nil {
		return nil, err
	}
	var tree json.RawMessage = raw
	return json.Marshal(map[string]any{
		"kind":   "tree_snapshot",
		"reason": "subscribe",
		"tree":   tree,
	})
}

// writeSSEEvent emits one SSE message: optional id (the event seq), one JSON
// data line, blank line terminator.
func writeSSEEvent(w http.ResponseWriter, f http.Flusher, id string, data []byte, secrets []string) {
	data = RedactBytes(data, secrets...)
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	// json.Marshal guarantees a single line; strings.ReplaceAll is a
	// belt-and-braces guard against accidental embedded newlines.
	fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(data), "\n", " "))
	f.Flush()
}

// tailLines returns at most n trailing non-empty lines of path, streaming the
// file through a ring buffer so a 10k-event log does not balloon memory.
func tailLines(path string, n int) ([][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	ring := make([][]byte, 0, n)
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), sseEventMaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		cp := append([]byte(nil), line...)
		if len(ring) < n {
			ring = append(ring, cp)
			continue
		}
		copy(ring, ring[1:])
		ring[n-1] = cp
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}
