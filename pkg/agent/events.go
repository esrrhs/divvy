package agent

import (
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
type EventRecorder struct {
	mu sync.Mutex
	f  *os.File
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &EventRecorder{f: f}, nil
}

// Close flips the underlying file.
func (r *EventRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// Record appends one event. Fields may override the built-in ts/kind keys.
func (r *EventRecorder) Record(kind, nodeID string, fields map[string]any) {
	if r == nil {
		return
	}
	ev := make(map[string]any, len(fields)+3)
	ev["ts"] = time.Now().Format(time.RFC3339Nano)
	ev["kind"] = kind
	if nodeID != "" {
		ev["node_id"] = nodeID
	}
	for k, v := range fields {
		ev[k] = v
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.f.Write(append(data, '\n'))
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
