package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
)

// EventRecorder persists one JSON object per line (JSONL) into the session's
// event stream. The file is opened append-only, so a crash still leaves a
// readable prefix and resumed sessions continue the same history. All methods
// are nil-safe so call sites never need nil checks.
//
// Besides the file, a recorder fans every event out to in-process subscribers
// (the web UI's SSE stream). Fan-out is best-effort and never blocks the
// orchestration loop: a slow subscriber drops events and receives a synthetic
// "_dropped" event reporting how many it missed.
type EventRecorder struct {
	mu sync.Mutex
	f  *os.File

	// seq is the monotonic event number, shared by the JSONL file and live
	// subscribers. SSE replays the file tail and then skips live events at
	// or below the last replayed seq, so reconnects neither gap nor dup.
	seq int64

	subMu sync.Mutex
	subs  map[int]*eventSubscription
	subID int

	// secrets scrubbed from every event before it is persisted (and before
	// fan-out): defense in depth on top of the tool subprocess env scrub —
	// any event field echoing tool output (output_preview, messages) is
	// guaranteed clean on disk.
	secrets []string
}

// secretMask is the replacement persisted/fanned out in place of a secret.
const secretMask = "***REDACTED***"

// AddSecret registers a credential value that must never appear in an event
// on disk or in a fan-out payload. Values under 4 chars are ignored so short
// placeholders cannot wipe single characters.
func (r *EventRecorder) AddSecret(v string) {
	if r == nil {
		return
	}
	v = strings.TrimSpace(v)
	if len(v) < 4 {
		return
	}
	r.mu.Lock()
	r.secrets = append(r.secrets, v)
	r.mu.Unlock()
}

// scrubSecrets recursively replaces registered secret values inside an event
// (strings, nested maps/slices, map keys, and RawMessage bytes) before
// persistence, returning the cleaned value.
func scrubSecrets(v any, secrets []string) any {
	switch t := v.(type) {
	case string:
		for _, s := range secrets {
			t = strings.ReplaceAll(t, s, secretMask)
		}
		return t
	case map[string]any:
		for k, val := range t {
			nk := scrubSecrets(k, secrets).(string)
			nv := scrubSecrets(val, secrets)
			if nk != k {
				delete(t, k)
			}
			t[nk] = nv
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = scrubSecrets(val, secrets)
		}
		return t
	case []byte: // json.RawMessage payloads (embedded tree snapshots)
		for _, s := range secrets {
			t = bytes.ReplaceAll(t, []byte(s), []byte(secretMask))
		}
		return t
	}
	return v
}

// eventBuffer is the per-subscriber channel capacity.
const eventBuffer = 128

type eventSubscription struct {
	ch      chan map[string]any
	dropped int
}

// Subscribe registers a live fan-out of every subsequently recorded event.
// The returned cancel detaches the subscriber; the channel is closed after
// cancellation or recorder Close. Callers must always call cancel to avoid
// leaking goroutines.
func (r *EventRecorder) Subscribe() (<-chan map[string]any, func()) {
	if r == nil {
		ch := make(chan map[string]any)
		return ch, func() { close(ch) }
	}
	r.subMu.Lock()
	r.subID++
	id := r.subID
	sub := &eventSubscription{ch: make(chan map[string]any, eventBuffer)}
	if r.subs == nil {
		r.subs = make(map[int]*eventSubscription)
	}
	r.subs[id] = sub
	r.subMu.Unlock()
	cancel := func() {
		r.subMu.Lock()
		if s, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(s.ch)
		}
		r.subMu.Unlock()
	}
	return sub.ch, cancel
}

// SubscriberCount reports how many live fan-out targets exist (tests/metrics).
func (r *EventRecorder) SubscriberCount() int {
	if r == nil {
		return 0
	}
	r.subMu.Lock()
	defer r.subMu.Unlock()
	return len(r.subs)
}

// fanOut delivers one event to every subscriber without blocking: when a
// subscriber buffer is full the event is counted as dropped for it.
func (r *EventRecorder) fanOut(ev map[string]any) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for _, s := range r.subs {
		if s.dropped > 0 {
			note := map[string]any{"kind": "_dropped", "count": s.dropped}
			select {
			case s.ch <- note:
				s.dropped = 0
			default:
				s.dropped++ // note itself could not be queued; keep counting
				continue
			}
		}
		select {
		case s.ch <- ev:
		default:
			s.dropped++
		}
	}
}

// Per-field size caps: the goal is post-mortem traceability, not full
// transcripts (full prompts/results can contain secrets and bloat the file).
const (
	evMessagePreview = 200
	evRespPreview    = 300
	evArgsChars      = 800
	evOutputChars    = 400
	evVerifyTail     = 1200
	evReasonChars    = 1500
	evMaxSummaries   = 24 // first 2 + last 20 messages summarized per call
)

// NewEventRecorder opens (creating the parent directory) the JSONL stream.
func NewEventRecorder(path string) (*EventRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// Drop a half-written final line (crash mid-write): it is unreadable
	// JSON and keeping it would make the next event reuse its seq, leaving a
	// torn duplicate line in the file.
	trimPartialLine(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	// A resumed run opens a file earlier runs wrote. Continue numbering from
	// the highest seq on disk instead of restarting at 1: SSE consumers use
	// seq both for replay/live de-duplication and Last-Event-ID resumes, so a
	// restarted counter would make the new run's events collide with earlier
	// runs' events (dropped as duplicates, stale terminal state winning).
	return &EventRecorder{f: f, seq: highestEventSeq(path)}, nil
}

// partialLineTail bounds how much of a file is inspected for a torn end.
const partialLineTail = 64 * 1024

// trimPartialLine truncates a file whose final line is not newline-terminated
// back to the end of the last complete line. No-op for empty/complete files.
func trimPartialLine(path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}
	size := info.Size()
	start := int64(0)
	if size > partialLineTail {
		start = size - partialLineTail
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return
	}
	if buf[len(buf)-1] == '\n' {
		return
	}
	idx := bytes.LastIndexByte(buf, '\n')
	if idx < 0 {
		_ = f.Truncate(0)
		return
	}
	_ = f.Truncate(start + int64(idx) + 1)
}

// eventSeqTail bounds how much of a possibly large JSONL file is scanned to
// resume the sequence. Events are clipped to a few KB, so 1 MiB always covers
// many events; if the file is smaller it is read in full.
const eventSeqTail = 1 << 20

func highestEventSeq(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var start int64
	if info, err := f.Stat(); err == nil && info.Size() > eventSeqTail {
		start = info.Size() - eventSeqTail
	}
	if _, err := f.Seek(start, 0); err != nil {
		return 0
	}
	sc := bufio.NewScanner(f)
	// Tree snapshot lines can be large; mirror the SSE replay ceiling.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var highest int64
	for sc.Scan() {
		var head struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue // tolerate a torn final line
		}
		if head.Seq > highest {
			highest = head.Seq
		}
	}
	return highest
}

// Close flips the underlying file and detaches all subscribers.
func (r *EventRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	err := r.f.Close()
	r.mu.Unlock()

	r.subMu.Lock()
	for id, s := range r.subs {
		close(s.ch)
		delete(r.subs, id)
	}
	r.subMu.Unlock()
	return err
}

// Record appends one event. Fields may override the built-in ts/kind keys.
func (r *EventRecorder) Record(kind, nodeID string, fields map[string]any) {
	if r == nil {
		return
	}
	ev := make(map[string]any, len(fields)+4)
	ev["ts"] = time.Now().Format(time.RFC3339Nano)
	ev["kind"] = kind
	if nodeID != "" {
		ev["node_id"] = nodeID
	}
	for k, v := range fields {
		ev[k] = v
	}
	// Assign the sequence and persist before fan-out while holding the same
	// lock: a subscriber that reconnects can trust every seq on disk and
	// never observe an event number the file does not contain.
	r.mu.Lock()
	if len(r.secrets) > 0 {
		ev = scrubSecrets(ev, r.secrets).(map[string]any)
	}
	r.seq++
	ev["seq"] = r.seq
	data, err := json.Marshal(ev)
	if err != nil {
		r.mu.Unlock()
		return
	}
	_, _ = r.f.Write(append(data, '\n'))
	r.mu.Unlock()
	r.fanOut(ev)
}

// clip trims and hard-cuts s to n chars, reporting how much was dropped.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(+" + strconv.Itoa(len(s)-n) + " chars)"
}

func tailClip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "(+" + strconv.Itoa(len(s)-n) + " chars)...\n" + s[len(s)-n:]
}

// summarizeMessages builds compact per-message descriptors. Long conversations
// keep the first 2 (system + original user) and last 20 messages, with the
// dropped middle count reported.
func summarizeMessages(msgs []llm.Message) []map[string]any {
	out := make([]map[string]any, 0, evMaxSummaries+1)
	add := func(m llm.Message) {
		e := map[string]any{
			"role": m.Role,
			"len":  len(m.Content),
		}
		if m.Name != "" {
			e["name"] = m.Name
		}
		if p := clip(m.Content, evMessagePreview); p != "" {
			e["preview"] = p
		}
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			e["tool_calls"] = names
		}
		out = append(out, e)
	}

	switch {
	case len(msgs) <= evMaxSummaries:
		for _, m := range msgs {
			add(m)
		}
	default:
		for _, m := range msgs[:2] {
			add(m)
		}
		out = append(out, map[string]any{"note": strconv.Itoa(len(msgs)-evMaxSummaries) + " earlier messages omitted"})
		for _, m := range msgs[len(msgs)-(evMaxSummaries-2):] {
			add(m)
		}
	}
	return out
}

// LLMCall records one foreman/decompose/worker model call with timing, usage,
// and content summaries (never the full transcript).
func (r *EventRecorder) LLMCall(kind, nodeID string, req llm.Request, resp *llm.Response, start time.Time, err error) {
	fields := map[string]any{
		"call_kind":   kind,
		"model":       req.Model,
		"n_messages":  len(req.Messages),
		"n_tools":     len(req.Tools),
		"duration_ms": time.Since(start).Milliseconds(),
		"messages":    summarizeMessages(req.Messages),
	}
	if err != nil {
		fields["error"] = clip(err.Error(), evReasonChars)
	}
	if resp != nil {
		fields["prompt_tokens"] = resp.Usage.PromptTokens
		fields["completion_tokens"] = resp.Usage.CompletionTokens
		fields["total_tokens"] = resp.Usage.TotalTokens
		if p := clip(resp.Content, evRespPreview); p != "" {
			fields["resp_preview"] = p
		}
		if len(resp.ToolCalls) > 0 {
			names := make([]string, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			fields["resp_tool_calls"] = names
		}
		if resp.Finish != "" {
			fields["finish_reason"] = resp.Finish
		}
	}
	r.Record("llm_call", nodeID, fields)
}

// EventSummary is a post-mortem roll-up of one session's JSONL event stream.
// The tree already records where the run *ended*; this records how it got
// there — how many attempts, retries and interventions it took.
type EventSummary struct {
	Counts     map[string]int // event kind -> occurrences
	LLMCalls   int
	ToolCalls  int
	ToolErrors int
	VerifyFail int
	Retries    int
	Stalls     int
	Resplits   int
	GiveUps    int
	Timeouts   int
	Outcome    string    // from the newest session_end, "" if the run never ended cleanly
	DurationMS int64     // wall clock of the newest session_end
	FirstTS    time.Time // newest session_start, i.e. the latest entry point
	LastTS     time.Time
	Malformed  int // lines that were not valid JSON objects
	Available  bool
}

// SummarizeEvents reads one session's event stream and rolls it up. A missing
// or unreadable stream is not an error: reports degrade to tree-only.
func SummarizeEvents(path string) (EventSummary, error) {
	s := EventSummary{Counts: map[string]int{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			s.Malformed++
			continue
		}
		s.Available = true
		kind, _ := ev["kind"].(string)
		s.Counts[kind]++
		switch kind {
		case "llm_call":
			s.LLMCalls++
		case "tool_call":
			s.ToolCalls++
			if ok, isBool := ev["ok"].(bool); isBool && !ok {
				s.ToolErrors++
			}
		case "verify":
			if ok, isBool := ev["ok"].(bool); isBool && !ok {
				s.VerifyFail++
			}
		case "retry":
			s.Retries++
		case "leaf_stall":
			s.Stalls++
		case "leaf_resplit":
			s.Resplits++
		case "leaf_giveup":
			s.GiveUps++
		case "leaf_timeout":
			s.Timeouts++
		case "session_start":
			if ts, ok := eventTime(ev); ok {
				s.FirstTS = ts
			}
		case "session_end":
			if ts, ok := eventTime(ev); ok {
				s.LastTS = ts
			}
			if v, ok := ev["outcome"].(string); ok {
				s.Outcome = v
			}
			if v, ok := ev["duration_ms"].(float64); ok {
				s.DurationMS = int64(v)
			}
		}
		if ts, ok := eventTime(ev); ok && (s.LastTS.IsZero() || ts.After(s.LastTS)) {
			s.LastTS = ts
		}
	}
	return s, nil
}

// eventTime parses the RFC3339 timestamp recorded on every event.
func eventTime(ev map[string]any) (time.Time, bool) {
	v, ok := ev["ts"].(string)
	if !ok {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// Total returns the number of events of any kind.
func (s EventSummary) Total() int {
	n := 0
	for _, v := range s.Counts {
		n += v
	}
	return n
}
func (r *EventRecorder) ToolCall(nodeID, tool string, args map[string]any, out string, callErr error, start time.Time) {
	fields := map[string]any{
		"tool":        tool,
		"duration_ms": time.Since(start).Milliseconds(),
		"output_len":  len(out),
	}
	if raw, err := json.Marshal(args); err == nil {
		fields["args"] = clip(string(raw), evArgsChars)
	}
	if callErr != nil {
		fields["ok"] = false
		fields["error"] = clip(callErr.Error(), evReasonChars)
		fields["output_preview"] = clip(out, evOutputChars)
	} else {
		fields["ok"] = true
		if p := clip(out, evOutputChars); p != "" {
			fields["output_preview"] = p
		}
	}
	r.Record("tool_call", nodeID, fields)
}
