package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
)

// sessionParams is the create/resume request body. Every tuning field is
// optional: absent fields fall back to DefaultConfig, which already carries
// OPENAI_* environment values. The API key never appears in any response.
type sessionParams struct {
	Goal         string  `json:"goal"`
	WorkDir      string  `json:"workdir"`
	Model        string  `json:"model"`
	BaseURL      string  `json:"base_url"`
	APIKey       string  `json:"api_key"`
	Parallel     int     `json:"parallel"`
	Isolate      bool    `json:"isolate"`
	GitCommit    bool    `json:"git_commit"`
	Web          bool    `json:"web"`
	Browser      bool    `json:"browser"`
	SearchURL    string  `json:"search_url"`
	ApprovalMode string  `json:"approval_mode"`
	BudgetTokens int     `json:"budget_tokens"`
	MaxCost      float64 `json:"max_cost"`
	MaxRetries   int     `json:"max_retries"`
	MaxDepth     int     `json:"max_depth"`
	MaxSubtasks  int     `json:"max_subtasks"`
	MaxStall     int     `json:"max_stall"`
	MaxTokens    int     `json:"max_tokens"`
	Temperature  float64 `json:"temperature"`
}

const maxLogPageLines = 2000

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var p sessionParams
	if err := decodeBody(r, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(p.Goal) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "goal is required"})
		return
	}
	cfg := s.configFromParams(p)
	client := s.newClient(cfg)
	h, err := s.mgr.Start(cfg, client, agent.SilentLogger())
	if err != nil {
		writeRunError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id": h.SessionID(),
		"workdir":    h.Workdir(),
		"phase":      h.Snapshot(),
	})
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p sessionParams
	if r.ContentLength > 0 {
		if err := decodeBody(r, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	cfg := s.configFromParams(p)
	cfg.SessionID = id
	cfg.Goal = "" // Load restores the goal from the persisted tree.
	client := s.newClient(cfg)
	h, err := s.mgr.Resume(cfg, client, agent.SilentLogger())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": h.SessionID(),
		"workdir":    h.Workdir(),
		"phase":      h.Snapshot(),
	})
}

// configFromParams layers request parameters over DefaultConfig (env-driven),
// pinning server-appropriate behavior (no token streaming, fast retries in
// tests are NOT forced here — CLI defaults stand).
func (s *Server) configFromParams(p sessionParams) agent.Config {
	cfg := agent.DefaultConfig()
	cfg.DataDir = s.dataDir
	cfg.WorkDir = s.workdir
	cfg.Stream = false
	if p.WorkDir != "" {
		cfg.WorkDir = p.WorkDir
	}
	cfg.Goal = p.Goal
	if p.Model != "" {
		cfg.Model = p.Model
	}
	if p.BaseURL != "" {
		cfg.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	if p.APIKey != "" {
		cfg.APIKey = p.APIKey
	}
	if p.Parallel > 0 {
		cfg.Parallel = p.Parallel
	}
	cfg.Isolate = p.Isolate
	cfg.GitCommit = p.GitCommit
	cfg.WebEnabled = p.Web
	cfg.BrowserEnabled = p.Browser
	if p.SearchURL != "" {
		cfg.SearchURL = p.SearchURL
	}
	if p.ApprovalMode != "" {
		cfg.LeafApproval = p.ApprovalMode
	}
	// Manual review cannot discard rejected work without isolation; enable it
	// rather than rejecting the request, and note it on the config.
	if cfg.LeafApprovalMode() == agent.LeafApprovalManual {
		cfg.Isolate = true
	}
	if p.BudgetTokens > 0 {
		cfg.BudgetTokens = p.BudgetTokens
	}
	if p.MaxCost > 0 {
		cfg.MaxCost = p.MaxCost
	}
	if p.MaxRetries > 0 {
		cfg.MaxRetries = p.MaxRetries
	}
	if p.MaxDepth > 0 {
		cfg.MaxDepth = p.MaxDepth
	}
	if p.MaxSubtasks > 0 {
		cfg.MaxSubtasks = p.MaxSubtasks
	}
	if p.MaxStall > 0 {
		cfg.MaxStall = p.MaxStall
	}
	if p.MaxTokens > 0 {
		cfg.MaxTokens = p.MaxTokens
	}
	if p.Temperature > 0 {
		cfg.Temperature = p.Temperature
	}
	return cfg
}

func newLLMClient(cfg agent.Config) llm.Client {
	c := llm.NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.RequestTimeout)
	c.ExtraJSON = cfg.ExtraJSON
	c.MaxBackoff = cfg.RetryMaxInterval
	return c
}

// sessionListItem merges on-disk metadata with the live handle's phase.
type sessionListItem struct {
	ID         string               `json:"id"`
	Goal       string               `json:"goal"`
	UpdatedAt  time.Time            `json:"updated_at"`
	RootState  string               `json:"root_state"`
	LeavesDone int                  `json:"leaves_done"`
	LeavesAll  int                  `json:"leaves_all"`
	Live       *agent.PhaseSnapshot `json:"live,omitempty"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	storage, err := engine.NewStorage(s.dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	infos, err := storage.ListSessions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]sessionListItem, 0, len(infos))
	for _, info := range infos {
		item := sessionListItem{
			ID:         info.ID,
			Goal:       info.Goal,
			UpdatedAt:  info.UpdatedAt,
			RootState:  string(info.RootState),
			LeavesDone: info.LeavesDone,
			LeavesAll:  info.LeavesAll,
		}
		if h := s.mgr.Get(info.ID); h != nil {
			snap := h.Snapshot()
			item.Live = &snap
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h := s.mgr.Get(id); h != nil {
		data, err := h.Orchestrator().Tree().ToJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": id,
			"workdir":    h.Workdir(),
			"live":       h.Snapshot(),
			"tree":       json.RawMessage(data),
		})
		return
	}

	storage, err := engine.NewStorage(s.dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tree, err := storage.LoadTree(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	data, err := tree.ToJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id,
		"workdir":    tree.WorkDir,
		"tree":       json.RawMessage(data),
	})
}

// ---- run control actions -------------------------------------------------

func (s *Server) handlePlanApprove(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	writeActionResult(w, h, h.ApprovePlan())
}

func (s *Server) handlePlanAdjust(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	var body struct {
		Comment string `json:"comment"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeActionResult(w, h, h.AdjustPlan(body.Comment))
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	writeActionResult(w, h, h.Abort())
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	writeActionResult(w, h, h.Pause())
}

func (s *Server) handleRedo(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	var body struct {
		NodeID string `json:"node_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeActionResult(w, h, h.Redo(body.NodeID))
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	var body struct {
		Instruction string `json:"instruction"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeActionResult(w, h, h.AddInstruction(body.Instruction))
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeActionResult(w, h, h.Answer(body.Text))
}

// ---- leaf diff approval --------------------------------------------------

func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	leafID := r.PathValue("leafID")
	req, found := h.PendingApproval(leafID)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no pending approval for leaf " + leafID,
		})
		return
	}
	payload := map[string]any{
		"session_id": h.SessionID(),
		"node_id":    req.NodeID,
		"title":      req.Title,
		"changes":    req.Changes,
	}
	data, merr := json.Marshal(payload)
	if merr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": merr.Error()})
		return
	}
	// P2-3: a diff whose Old/NewContent echoes a secret must be masked like
	// the SSE path (defense in depth; upstream recorders already scrub).
	data = RedactBytes(data, h.SecretValues()...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	h, ok := s.liveOr404(w, r)
	if !ok {
		return
	}
	leafID := r.PathValue("leafID")
	var body struct {
		Decision string `json:"decision"` // "approve" | "reject"
		Comment  string `json:"comment"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var approved bool
	switch strings.ToLower(strings.TrimSpace(body.Decision)) {
	case "approve", "approved":
		approved = true
	case "reject", "rejected":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `decision must be "approve" or "reject"`,
		})
		return
	}
	writeActionResult(w, h, h.DecideLeaf(leafID, approved, body.Comment))
}

// ---- log paging ----------------------------------------------------------

func (s *Server) handleSessionLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	logPath := filepath.Join(s.dataDir, "logs", id+".log")
	if _, err := os.Stat(logPath); err != nil {
		// Live sessions can be at plan review before the first log line
		// lands; treat a missing file as an empty page.
		if s.mgr.Get(id) != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"lines": []string{}, "offset": 0, "next_offset": 0, "has_more": false,
			})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session log not found"})
		return
	}
	offset := queryInt(r, "offset", 0)
	limit := queryInt(r, "limit", 500)
	if offset < 0 || limit <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offset must be >=0 and limit >0"})
		return
	}
	if limit > maxLogPageLines {
		limit = maxLogPageLines
	}

	f, err := os.Open(logPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()

	var secrets []string
	if lh := s.mgr.Get(id); lh != nil {
		secrets = lh.SecretValues()
	}
	lines := make([]string, 0, limit)
	total := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if total >= offset && len(lines) < limit {
			lines = append(lines, Redact(sc.Text(), secrets...))
		}
		total++
	}
	if err := sc.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	nextOffset := offset + len(lines)
	writeJSON(w, http.StatusOK, map[string]any{
		"lines":       lines,
		"offset":      offset,
		"next_offset": nextOffset,
		"total":       total,
		"has_more":    nextOffset < total,
	})
}

// ---- helpers -------------------------------------------------------------

func (s *Server) liveOr404(w http.ResponseWriter, r *http.Request) (*agent.RunHandle, bool) {
	h := s.mgr.Get(r.PathValue("id"))
	if h == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session is not running"})
		return nil, false
	}
	return h, true
}

func writeActionResult(w http.ResponseWriter, h *agent.RunHandle, err error) {
	if err != nil {
		writeRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phase": h.Snapshot()})
}

// writeRunError maps the run manager's sentinel errors onto REST status
// codes: conflicts/state protection → 409, bad arguments → 400.
func writeRunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrWorkdirBusy),
		errors.Is(err, agent.ErrSessionActive),
		errors.Is(err, agent.ErrRunFinished),
		errors.Is(err, agent.ErrWrongPhase):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// Unknown fields are ignored so a newer frontend stays compatible.
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
