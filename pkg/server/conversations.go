package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
)

// conversationView is the API shape: settings and messages, plus whether
// this process still holds an API key. The key itself is never included.
type conversationView struct {
	Conversation
	HasAPIKey       bool                 `json:"has_api_key"`
	ContextNote     string               `json:"context_note"`
	RunPhase        string               `json:"run_phase,omitempty"`
	RunSessionID    string               `json:"run_session_id,omitempty"`
	KeyRequired     bool                 `json:"key_required,omitempty"`
	AgentLoop       string               `json:"agent_loop,omitempty"`
	PendingInject   string               `json:"pending_inject,omitempty"`
	InjectState     string               `json:"inject_state,omitempty"`
	InTool          bool                 `json:"in_tool,omitempty"`
	PendingAsk      string               `json:"pending_ask,omitempty"`
	LiveAssistant   string               `json:"live_assistant,omitempty"`
	LiveStreamID    string               `json:"live_stream_id,omitempty"`
	LeafApproving   string               `json:"leaf_approving,omitempty"`
	PendingRollback *pendingRollbackView `json:"pending_rollback,omitempty"`
}

type createConversationRequest struct {
	sessionParams
	Message string `json:"message"`
	Title   string `json:"title"`
}

type postMessageRequest struct {
	Text       string          `json:"text"`
	APIKey     string          `json:"api_key"`
	Paths      []string        `json:"paths"`
	Selections []codeSelection `json:"selections"`
	Mode       string          `json:"mode"` // start | queue | takeover | steer
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	list, err := listConversations(s.dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	views := make([]conversationView, 0, len(list))
	for _, c := range list {
		views = append(views, s.viewConversation(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": views})
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req createConversationRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	workdir := strings.TrimSpace(req.WorkDir)
	if workdir == "" {
		workdir = s.workdir
	}
	abs, err := resolveExistingDir(workdir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings := settingsFromParams(req.sessionParams)
	c, err := newConversation(abs, settings, req.Title)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key != "" {
		s.setAPIKey(c.ID, key)
	}
	if err := saveConversation(s.dataDir, c, s.secretsFor(c.ID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	msg := strings.TrimSpace(req.Message)
	if msg != "" {
		if err := s.startConversationTurn(c, msg, key, nil, nil); err != nil {
			// The conversation itself was created; surface the start error
			// after reloading so the client still has an id to continue.
			updated, loadErr := loadConversation(s.dataDir, c.ID)
			if loadErr == nil {
				c = updated
			}
			writeJSON(w, statusForConvErr(err), map[string]any{
				"error":        err.Error(),
				"conversation": s.viewConversation(c),
			})
			return
		}
		reloaded, err := loadConversation(s.dataDir, c.ID)
		if err == nil {
			c = reloaded
		}
	}
	writeJSON(w, http.StatusCreated, s.viewConversation(c))
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.viewConversation(c))
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.loadConv(id)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	if phase, _ := s.conversationRun(c); phase == string(agent.PhasePlanning) ||
		phase == string(agent.PhasePlanReview) ||
		phase == string(agent.PhaseRunning) ||
		phase == string(agent.PhaseLeafApproval) ||
		phase == string(agent.PhaseAsk) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "conversation has a running session; pause or abort it before deleting",
		})
		return
	}
	if err := deleteConversationFile(s.dataDir, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.clearAPIKey(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req postMessageRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	if key := strings.TrimSpace(req.APIKey); key != "" {
		s.setAPIKey(c.ID, key)
	}
	if err := s.dispatchMessage(c, text, strings.TrimSpace(req.APIKey), req.Paths, req.Selections, req.Mode); err != nil {
		s.writeConvConflict(w, c, err)
		return
	}
	reloaded, err := loadConversation(s.dataDir, c.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, s.viewConversation(reloaded))
}

func (s *Server) handleSetConversationKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		s.clearAPIKey(c.ID)
	} else {
		s.setAPIKey(c.ID, key)
	}
	writeJSON(w, http.StatusOK, s.viewConversation(c))
}

func (s *Server) handleResumeConversation(w http.ResponseWriter, r *http.Request) {
	var req postMessageRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	if key := strings.TrimSpace(req.APIKey); key != "" {
		s.setAPIKey(c.ID, key)
	}
	if reason, blocked := s.rollbackWriteBlock(c); blocked {
		s.writeConvConflict(w, c, errConvConflict(reason))
		return
	}
	phase, sid := s.conversationRun(c)
	if sid == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可续跑的会话"})
		return
	}
	if phase != "" && phase != string(agent.PhasePaused) && phase != string(agent.PhaseFailed) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "当前任务仍在运行或已完成，不能续跑；完成或失败后请直接发送新需求",
		})
		return
	}
	// Failed sessions are not resumed in place — the user should send a
	// new message. Only a paused (or interrupted) session continues.
	if phase != string(agent.PhasePaused) {
		// Handle may already have been forgotten; a persisted incomplete
		// tree is still resumable via the session resume API.
		if phase == string(agent.PhaseFailed) || phase == string(agent.PhaseDone) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "当前任务已结束。请在同一对话里发送新的修改需求，而不是续跑。",
			})
			return
		}
	}

	cfg, err := s.configForConversation(c)
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	cfg.SessionID = sid
	cfg.Goal = ""
	cfg.HoldServe = true
	client := s.newClient(cfg)
	h, err := s.mgr.Resume(cfg, client, agent.SilentLogger())
	if err != nil {
		writeRunError(w, err)
		return
	}
	s.bindSession(c.ID, h.SessionID())
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	c.appendMessage("system", kindNotice, "已续跑当前任务，继续上次未完成的执行。", h.SessionID(), "", secrets)
	c.CurrentSessionID = h.SessionID()
	saveErr := saveConversation(s.dataDir, c, secrets)
	if _, ok := s.snaps[h.SessionID()]; !ok {
		s.mu.Unlock()
		if snap, serr := snapshotWorkdir(c.WorkDir); serr == nil {
			s.mu.Lock()
			s.snaps[h.SessionID()] = snap
			s.mu.Unlock()
		}
		s.mu.Lock()
	}
	done := make(chan struct{})
	s.watchDone[c.ID] = done
	s.mu.Unlock()
	s.armConversationFinalize(c.ID, h)
	if saveErr != nil {
		_ = h.Abort()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": saveErr.Error()})
		return
	}
	go s.watchConversationRun(c.ID, h, done)
	go s.watchPendingSteer(c.ID)
	h.Launch()

	reloaded, _ := loadConversation(s.dataDir, c.ID)
	if reloaded == nil {
		reloaded = c
	}
	writeJSON(w, http.StatusOK, s.viewConversation(reloaded))
}

func activeRunPhase(phase string) bool {
	switch phase {
	case string(agent.PhasePlanning), string(agent.PhasePlanReview),
		string(agent.PhaseRunning), string(agent.PhaseLeafApproval),
		string(agent.PhaseAsk):
		return true
	}
	return false
}

func (s *Server) dispatchMessage(c *Conversation, text, requestKey string, paths []string, sels []codeSelection, mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "start"
	}
	switch mode {
	case "start", "queue", "takeover", "steer":
	default:
		return fmt.Errorf("unknown mode %q (use start, queue, takeover, or steer)", mode)
	}
	if reason, blocked := s.rollbackWriteBlock(c); blocked {
		return errConvConflict(reason)
	}
	phase, _ := s.conversationRun(c)
	running := activeRunPhase(phase)
	paused := phase == string(agent.PhasePaused)

	if mode == "start" && (running || paused) {
		if running {
			mode = "steer"
		}
	}
	if mode == "queue" && paused {
		return errConvConflict("当前任务已暂停。请先续跑，或改用「中止并接管」发送新需求（暂停期间不会自动开跑排队项）")
	}
	if mode == "queue" && !running {
		mode = "start"
	}
	if mode == "queue" {
		return s.enqueueTurn(c, text, paths, sels)
	}
	if mode == "steer" {
		if running || paused {
			return s.steerCurrentRun(c, text, paths, sels)
		}
		// Nothing live to inject into: start a new turn instead.
		mode = "start"
	}
	if mode == "takeover" && (running || paused) {
		if err := s.abortCurrentRun(c); err != nil {
			return err
		}
		// Reload after the watcher has recorded the abort.
		reloaded, err := loadConversation(s.dataDir, c.ID)
		if err == nil {
			*c = *reloaded
		}
	}
	if mode == "start" && paused {
		return errConvConflict("当前任务已暂停。请先续跑，或使用「中止并接管」用新指令开始")
	}
	return s.startConversationTurn(c, text, requestKey, paths, sels)
}

func (s *Server) enqueueTurn(c *Conversation, text string, paths []string, sels []codeSelection) error {
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := loadConversation(s.dataDir, c.ID)
	if err == nil {
		*c = *latest
	}
	replaced := c.Queue != nil
	c.Queue = &QueuedTurn{Text: text, Paths: paths, Selections: sels, CreatedAt: time.Now().UTC()}
	msg := "已排队：当前任务完成后（成功、失败或中止）会自动开始这一条。暂停不会自动开跑。新的排队会替换尚未开始的上一条。"
	if replaced {
		msg = "已更新排队中的下一条需求。当前任务完成后会自动开始；可用「取消排队」撤销。"
	}
	c.appendMessage("user", kindUser, text, "", "", secrets)
	c.Messages[len(c.Messages)-1].Attachments = mergeFileRefs(refsFromPaths(paths), refsFromSelections(sels))
	c.appendMessage("system", kindNotice, msg, "", "queued", secrets)
	return saveConversation(s.dataDir, c, secrets)
}

func refsFromPaths(paths []string) []fileRef {
	out := make([]fileRef, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, fileRef{Path: p, Type: "file"})
	}
	return out
}

func (s *Server) abortCurrentRun(c *Conversation) error {
	_, sid := s.conversationRun(c)
	if sid == "" {
		return nil
	}
	h := s.mgr.Get(sid)
	if h == nil || h.Phase().Terminal() {
		return nil
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	s.skipQueue[c.ID] = true
	// Drop a pending queue so abort-for-takeover does not also auto-start it.
	if latest, err := loadConversation(s.dataDir, c.ID); err == nil {
		latest.Queue = nil
		latest.Steer = nil
		_ = saveConversation(s.dataDir, latest, secrets)
	}
	ch := s.watchDone[c.ID]
	s.mu.Unlock()
	if err := h.Abort(); err != nil && !errors.Is(err, agent.ErrRunFinished) {
		return err
	}
	select {
	case <-h.Done():
	case <-time.After(15 * time.Second):
		return errConvConflict("中止当前任务超时，请稍后重试")
	}
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(8 * time.Second):
		}
	}
	s.mu.Lock()
	delete(s.skipQueue, c.ID)
	s.mu.Unlock()
	return nil
}

func (s *Server) startConversationTurn(c *Conversation, userText, requestKey string, paths []string, sels []codeSelection) error {
	phase, _ := s.conversationRun(c)
	if activeRunPhase(phase) {
		return errConvConflict("当前正在执行，请排队等待结束，或中止并用新指令接管")
	}
	if phase == string(agent.PhasePaused) {
		return errConvConflict("当前任务已暂停。请先续跑，或中止后接管")
	}
	if reason, blocked := s.rollbackWriteBlock(c); blocked {
		return errConvConflict(reason)
	}

	fileCtx, refs, err := collectContext(c.WorkDir, paths)
	if err != nil {
		return err
	}
	selCtx, selRefs, serr := collectSelections(sels)
	if serr != nil {
		return serr
	}
	fileCtx += selCtx
	refs = mergeFileRefs(refs, selRefs)
	autoCtx := collectProjectContext(c.WorkDir, userText)

	cfg, err := s.configForConversation(c)
	if err != nil {
		return err
	}
	if requestKey != "" {
		cfg.APIKey = requestKey
	}
	secrets := s.secretsFor(c.ID)
	if requestKey != "" {
		secrets = append(secrets, requestKey)
	}
	cfg.Goal = followUpGoal(
		Redact(userText, secrets...),
		Redact(conversationMemory(c), secrets...),
		Redact(fileCtx, secrets...),
		Redact(autoCtx, secrets...),
	)
	// Unique even when two turns start in the same second; the engine's
	// default sess_<YYYYMMDD_HHMMSS> would otherwise collide and reuse files.
	cfg.SessionID = newSessionID()
	cfg.WorkDir = c.WorkDir

	if conversationAgentMode(c) == agent.AgentLoopDirect && c.CurrentSessionID != "" {
		if done, exists, terr := agent.DirectTranscriptDone(cfg.TranscriptPath); terr == nil && exists && !done {
			return s.resumeDirectAndInject(c, userText, requestKey, paths, sels)
		}
	}

	// Snapshot and persist hooks are installed before Launch so a fast
	// scripted run cannot publish Done before the baseline and finalize
	// hook exist, and cannot write before the snapshot is stored.
	snap, snapErr := snapshotWorkdir(c.WorkDir)
	cfg.HoldServe = true
	client := s.newClient(cfg)
	h, err := s.mgr.Start(cfg, client, agent.SilentLogger())
	if err != nil {
		return err
	}
	s.bindSession(c.ID, h.SessionID())
	if snapErr == nil {
		s.mu.Lock()
		s.snaps[h.SessionID()] = snap
		s.mu.Unlock()
	}
	s.armConversationFinalize(c.ID, h)
	secrets = s.secretsFor(c.ID)

	s.mu.Lock()
	// Reload under the lock so a concurrent watch cannot clobber this turn.
	latest, loadErr := loadConversation(s.dataDir, c.ID)
	if loadErr == nil {
		*c = *latest
	}
	if strings.TrimSpace(c.Title) == "" || c.Title == baseName(c.WorkDir) {
		c.Title = oneLine(userText, 80)
	}
	c.appendMessage("user", kindUser, userText, h.SessionID(), "", secrets)
	if len(refs) > 0 {
		c.Messages[len(c.Messages)-1].Attachments = refs
	}
	c.appendMessage("system", kindRunStarted, startTurnNotice(c), h.SessionID(), "", secrets)
	c.CurrentSessionID = h.SessionID()
	if !containsString(c.SessionIDs, h.SessionID()) {
		c.SessionIDs = append(c.SessionIDs, h.SessionID())
	}
	saveErr := saveConversation(s.dataDir, c, secrets)
	done := make(chan struct{})
	s.watchDone[c.ID] = done
	s.mu.Unlock()
	if saveErr != nil {
		_ = h.Abort()
		return saveErr
	}
	go s.watchConversationRun(c.ID, h, done)
	h.Launch()
	return nil
}

func (s *Server) armConversationFinalize(convID string, h *agent.RunHandle) {
	if s == nil || h == nil {
		return
	}
	h.SetBeforeTerminal(func(phase agent.RunPhase) {
		s.finalizeConversationRun(convID, h, phase)
	})
}

func (s *Server) finalizeConversationRun(convID string, h *agent.RunHandle, phase agent.RunPhase) {
	outcome := runOutcome(string(phase), phase == agent.PhaseFailed)
	if phase == agent.PhaseFailed {
		if snap := h.Snapshot(); strings.Contains(strings.ToLower(snap.Error), "abort") {
			outcome = "aborted"
		}
	}
	result := ""
	if snap := h.Snapshot(); snap.Error != "" {
		result = snap.Error
	}
	var tree *engine.TaskTree
	if o := h.Orchestrator(); o != nil {
		tree = o.Tree()
	}

	s.mu.Lock()
	// An empty workdir yields a nil slice; use the comma-ok form so we still
	// diff against that baseline (otherwise added files would be missed).
	before, haveSnap := s.snaps[h.SessionID()]
	if outcome != "paused" {
		delete(s.snaps, h.SessionID())
	}
	skipQ := s.skipQueue[convID]
	s.mu.Unlock()

	var changes []turnChange
	if haveSnap {
		changes, _ = diffAgainstSnapshot(before, h.Workdir())
	}
	secrets := s.secretsFor(convID)
	if haveSnap {
		// Persist even when the list is empty so GET /changes can tell a
		// finished turn (editable, no diffs) from a missing record.
		_ = saveTurnChanges(s.dataDir, convID, h.SessionID(), changes, secrets)
	}

	s.mu.Lock()
	c, err := loadConversation(s.dataDir, convID)
	var queued *QueuedTurn
	if err == nil {
		userText := lastUserText(c, h.SessionID())
		summary := summarizeChanges(changes)
		changeNote := ""
		if n := len(summary); n > 0 {
			changeNote = fmt.Sprintf("改动 %d 个文件", n)
		}
		text := finishedMessageText(outcome, result, tree)
		if changeNote != "" {
			text = text + " " + changeNote + "。"
		}
		c.appendWorkLog(compactTurn(userText, outcome, joinChangeNames(summary), tree), secrets)
		if model := strings.TrimSpace(h.LastSummary()); model != "" {
			c.appendMessage("assistant", kindModelReply, model, h.SessionID(), "model", secrets)
		}
		c.appendMessage("system", kindRunFinished, text, h.SessionID(), outcome, secrets)
		c.Messages[len(c.Messages)-1].Changes = summary
		if leftover := c.Steer; leftover != nil && outcome != "paused" {
			c.Steer = nil
			// The follow-up missed this turn; run it next, ahead of any queue.
			queued = &QueuedTurn{Text: leftover.Text, Paths: leftover.Paths, Selections: leftover.Selections, CreatedAt: leftover.CreatedAt}
			c.appendMessage("system", kindNotice, "这条调整没赶上当前轮的注入窗口，已改为下一轮自动开始（已完成工作不受影响）。", "", "steer_deferred", secrets)
		} else if !skipQ && outcome != "paused" && c.Queue != nil {
			queued = c.Queue
			c.Queue = nil
		}
		_ = saveConversation(s.dataDir, c, secrets)
	}
	if queued != nil {
		s.queuedAfterRun[convID] = queued
	}
	s.mu.Unlock()
}

func (s *Server) watchConversationRun(convID string, h *agent.RunHandle, done chan struct{}) {
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	evCh, evCancel := h.SubscribeEvents()
	evDone := make(chan struct{})
	go func() {
		defer close(evDone)
		s.forwardDirectEvents(convID, evCh)
	}()
	<-h.Done()
	evCancel()
	<-evDone
	_ = h.Wait()
	s.startQueuedAfterRun(convID)
}

func (s *Server) startQueuedAfterRun(convID string) {
	s.mu.Lock()
	queued := s.queuedAfterRun[convID]
	delete(s.queuedAfterRun, convID)
	s.mu.Unlock()
	if queued == nil {
		return
	}
	latest, lerr := loadConversation(s.dataDir, convID)
	if lerr != nil {
		return
	}
	if err := s.startConversationTurn(latest, queued.Text, "", queued.Paths, queued.Selections); err != nil {
		secrets := s.secretsFor(convID)
		s.mu.Lock()
		if again, e2 := loadConversation(s.dataDir, convID); e2 == nil {
			again.Queue = queued
			again.appendMessage("system", kindNotice, "排队的需求未能自动开始："+err.Error(), "", "", secrets)
			_ = saveConversation(s.dataDir, again, secrets)
		}
		s.mu.Unlock()
	}
}

func joinChangeNames(changes []turnChangeSummary) string {
	if len(changes) == 0 {
		return ""
	}
	names := make([]string, 0, len(changes))
	for _, c := range changes {
		names = append(names, c.Status+":"+c.Path)
	}
	return strings.Join(names, ", ")
}

func (s *Server) configForConversation(c *Conversation) (agent.Config, error) {
	p := sessionParams{
		WorkDir:      c.WorkDir,
		Model:        c.Settings.Model,
		BaseURL:      c.Settings.BaseURL,
		Isolate:      c.Settings.Isolate,
		GitCommit:    c.Settings.GitCommit,
		Web:          c.Settings.Web,
		Browser:      c.Settings.Browser,
		ApprovalMode: c.Settings.ApprovalMode,
		BudgetTokens: c.Settings.BudgetTokens,
		MaxCost:      c.Settings.MaxCost,
		Parallel:     c.Settings.Parallel,
	}
	if key := s.getAPIKey(c.ID); key != "" {
		p.APIKey = key
	}
	cfg := s.configFromParams(p)
	cfg.WorkDir = c.WorkDir
	cfg.DataDir = s.dataDir
	cfg.Stream = false
	if conversationAgentMode(c) == agent.AgentLoopDirect {
		cfg.AgentLoop = agent.AgentLoopDirect
		cfg.TranscriptPath = conversationDirectPath(s.dataDir, c.ID)
		cfg.Stream = true
	}
	if cfg.RequiresAPIKey() && strings.TrimSpace(cfg.APIKey) == "" {
		return cfg, errConvNeedKey("当前模型需要 API key。服务重启后密钥不会保留，请在本对话中重新填写（不会写入历史）。")
	}
	return cfg, nil
}

func settingsFromParams(p sessionParams) ConversationSettings {
	mode := p.ApprovalMode
	if mode == "" {
		mode = agent.LeafApprovalAuto
	}
	isolate := p.Isolate
	if mode == agent.LeafApprovalManual {
		isolate = true
	}
	// Default isolate to on when the client omitted it as false but we want
	// a safe default for new conversations: the form always sends the
	// field. Callers that leave it zero get isolate=false only if they
	// explicitly stored it that way. New conversations from the UI send true.
	return ConversationSettings{
		Model:        p.Model,
		BaseURL:      p.BaseURL,
		Parallel:     p.Parallel,
		Isolate:      isolate,
		GitCommit:    p.GitCommit,
		Web:          p.Web,
		Browser:      p.Browser,
		ApprovalMode: mode,
		BudgetTokens: p.BudgetTokens,
		MaxCost:      p.MaxCost,
		AgentMode:    normalizeAgentMode(p.AgentMode),
	}
}

func (s *Server) viewConversation(c *Conversation) conversationView {
	phase, sid := s.conversationRun(c)
	cfg := s.configFromParams(sessionParams{
		Model:   c.Settings.Model,
		BaseURL: c.Settings.BaseURL,
	})
	keyRequired := cfg.RequiresAPIKey() && !s.hasAPIKey(c.ID) && strings.TrimSpace(cfg.APIKey) == ""
	v := conversationView{
		Conversation: *c,
		HasAPIKey:    s.hasAPIKey(c.ID) || strings.TrimSpace(cfg.APIKey) != "",
		ContextNote:  convContextNote,
		RunPhase:     phase,
		RunSessionID: sid,
		KeyRequired:  keyRequired,
		AgentLoop:    conversationAgentMode(c),
	}
	if sid != "" {
		if h := s.mgr.Get(sid); h != nil {
			snap := h.Snapshot()
			v.PendingInject = snap.PendingInject
			v.InTool = snap.InTool
			v.PendingAsk = snap.PendingAsk
			v.LiveAssistant = snap.LiveAssistant
			v.LiveStreamID = snap.LiveStreamID
			v.LeafApproving = snap.LeafApproving
			if snap.PendingInject != "" {
				v.InjectState = "pending"
			}
		} else if c.Steer != nil {
			v.PendingInject = c.Steer.Text
			v.InjectState = "pending"
		}
	}
	if j, err := loadRollbackJournal(s.dataDir, c.ID); err != nil {
		v.PendingRollback = &pendingRollbackView{
			Phase:   "unreadable",
			Error:   err.Error(),
			Message: journalUnreadableMessage(err.Error()),
		}
	} else if p := pendingRollbackFromJournal(j); p != nil {
		v.PendingRollback = p
	} else if j != nil && j.Phase == rollbackPhaseDone && !containsString(c.RolledBackTurns, j.SessionID) {
		v.PendingRollback = &pendingRollbackView{
			SessionID: j.SessionID,
			Phase:     j.Phase,
			Message:   "文件已恢复，对话记录尚未写完。请先完成恢复。",
		}
	}
	if v.PendingRollback == nil {
		if reason, blocked := s.rollbackPendingForWorkdir(c.WorkDir); blocked {
			v.PendingRollback = &pendingRollbackView{Message: reason}
		}
	}
	return v
}

func (s *Server) conversationRun(c *Conversation) (phase, sessionID string) {
	sessionID = c.CurrentSessionID
	if sessionID == "" && len(c.SessionIDs) > 0 {
		sessionID = c.SessionIDs[len(c.SessionIDs)-1]
	}
	if sessionID == "" {
		return "", ""
	}
	if h := s.mgr.Get(sessionID); h != nil {
		return string(h.Phase()), sessionID
	}
	// After a process restart the handle is gone. If a direct transcript
	// is still mid-loop, surface paused so the user can resume the same
	// tool context instead of starting a second turn.
	if conversationAgentMode(c) == agent.AgentLoopDirect && sessionID != "" {
		done, exists, err := agent.DirectTranscriptDone(conversationDirectPath(s.dataDir, c.ID))
		if err == nil && exists && !done {
			return string(agent.PhasePaused), sessionID
		}
	}
	return "", sessionID
}

func (s *Server) loadConv(id string) (*Conversation, error) {
	if !validConversationID(id) {
		return nil, errConvNotFound()
	}
	c, err := loadConversation(s.dataDir, id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errConvNotFound()
		}
		return nil, err
	}
	return c, nil
}

func (s *Server) setAPIKey(id, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apiKeys == nil {
		s.apiKeys = map[string]string{}
	}
	s.apiKeys[id] = key
}

func (s *Server) clearAPIKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.apiKeys, id)
}

func (s *Server) getAPIKey(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiKeys[id]
}

func (s *Server) hasAPIKey(id string) bool {
	return strings.TrimSpace(s.getAPIKey(id)) != ""
}

func (s *Server) secretsFor(id string) []string {
	var out []string
	if k := s.getAPIKey(id); k != "" {
		out = append(out, k)
	}
	return out
}

func (s *Server) bindSession(convID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionConv == nil {
		s.sessionConv = map[string]string{}
	}
	s.sessionConv[sessionID] = convID
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (s *Server) steerCurrentRun(c *Conversation, text string, paths []string, sels []codeSelection) error {
	secrets := s.secretsFor(c.ID)
	instruction := Redact(steerInstruction(c.WorkDir, text, paths, sels), secrets...)
	applied, how, injectID, err := s.applySteer(c, instruction)
	if err != nil && !isRetryableSteer(err) {
		return err
	}
	s.mu.Lock()
	latest, lerr := loadConversation(s.dataDir, c.ID)
	if lerr == nil {
		*c = *latest
	}
	c.appendMessage("user", kindUser, text, c.CurrentSessionID, "steered", secrets)
	c.Messages[len(c.Messages)-1].Attachments = mergeFileRefs(refsFromPaths(paths), refsFromSelections(sels))
	if applied {
		c.Steer = nil
		c.appendMessage("system", kindNotice, how, c.CurrentSessionID, "steered", secrets)
	} else {
		c.Steer = &PendingSteer{Text: text, Paths: paths, Selections: sels, InjectID: injectID, CreatedAt: time.Now().UTC()}
		msg := how
		if strings.TrimSpace(msg) == "" {
			msg = pendingSteerNotice(c)
		}
		c.appendMessage("system", kindNotice, msg, c.CurrentSessionID, "steer_pending", secrets)
	}
	saveErr := saveConversation(s.dataDir, c, secrets)
	s.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	if !applied {
		go s.watchPendingSteer(c.ID)
	}
	return nil
}

func steerInstruction(workdir, text string, paths []string, sels []codeSelection) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	if len(paths) > 0 {
		fileCtx, _, err := collectContext(workdir, paths)
		if err == nil && strings.TrimSpace(fileCtx) != "" {
			b.WriteString(fileCtx)
		}
	}
	if selCtx, _, err := collectSelections(sels); err == nil && strings.TrimSpace(selCtx) != "" {
		b.WriteString(selCtx)
	}
	out := b.String()
	if len(out) > 8<<10 {
		return out[:8<<10] + "\n…(injected instruction truncated)\n"
	}
	return out
}

func isRetryableSteer(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, agent.ErrWrongPhase) || errors.Is(err, agent.ErrRunFinished)
}

func (s *Server) applySteer(c *Conversation, instruction string) (applied bool, notice, injectID string, err error) {
	_, sid := s.conversationRun(c)
	if sid == "" {
		return false, "", "", nil
	}
	h := s.mgr.Get(sid)
	if h == nil {
		return false, "", "", nil
	}
	phase := string(h.Phase())
	switch phase {
	case string(agent.PhasePlanReview):
		err = h.AdjustPlan(instruction)
		if err != nil {
			return false, "", "", err
		}
		return true, "已把这条意见送入当前计划审查并立即重新规划；已完成的工作仍保留，审批通过后才会执行。", "", nil
	case string(agent.PhaseRunning), string(agent.PhaseAsk):
		if h.AgentLoop() == agent.AgentLoopDirect {
			injectID, err = h.InjectAtBoundaryID(instruction)
			if err != nil {
				return false, "", "", err
			}
			if h.InTool() {
				return false, "已记下这条调整，将在当前工具结束后的下一模型步骤注入同一循环（不会取消正在执行的工具，也不会丢掉本轮已完成的修改）。", injectID, nil
			}
			return false, "已记下这条调整，将在下一模型步骤注入同一循环（不会取消本轮或丢掉已完成的修改）。", injectID, nil
		}
		if phase == string(agent.PhaseAsk) {
			return false, "", "", nil
		}
		err = h.SteerNow(instruction)
		if err != nil {
			return false, "", "", err
		}
		return true, "已打断当前正在跑的叶子（该叶子尚未合并的改动会丢弃，已完成的叶子保留），并把这条指令立即加入当前轮，接下来就会执行。", "", nil
	default:
		return false, "", "", nil
	}
}

func (s *Server) stopSteerWaitLocked(id string) {
	if prev, ok := s.steerWait[id]; ok {
		delete(s.steerWait, id)
		select {
		case <-prev:
		default:
			close(prev)
		}
	}
}

func (s *Server) watchPendingSteer(convID string) {
	s.mu.Lock()
	s.stopSteerWaitLocked(convID)
	stop := make(chan struct{})
	s.steerWait[convID] = stop
	s.mu.Unlock()

	deadline := time.Now().Add(30 * time.Minute)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				return
			}
		}
		c, err := loadConversation(s.dataDir, convID)
		if err != nil || c == nil || c.Steer == nil {
			return
		}
		phase, _ := s.conversationRun(c)
		if phase == string(agent.PhasePaused) {
			continue
		}
		if !activeRunPhase(phase) {
			// watchConversationRun will defer the leftover steer to a new turn.
			return
		}
		if _, sid := s.conversationRun(c); sid != "" {
			if h := s.mgr.Get(sid); h != nil && strings.TrimSpace(h.PendingInject()) != "" {
				continue
			}
		}
		instruction := Redact(steerInstruction(c.WorkDir, c.Steer.Text, c.Steer.Paths, c.Steer.Selections), s.secretsFor(convID)...)
		applied, how, injectID, aerr := s.applySteer(c, instruction)
		if !applied {
			if aerr != nil && !isRetryableSteer(aerr) {
				secrets := s.secretsFor(convID)
				s.mu.Lock()
				if latest, e2 := loadConversation(s.dataDir, convID); e2 == nil && latest.Steer != nil {
					latest.appendMessage("system", kindNotice, "调整未能注入当前轮："+aerr.Error(), "", "steer_failed", secrets)
					_ = saveConversation(s.dataDir, latest, secrets)
				}
				s.mu.Unlock()
				return
			}
			if injectID != "" {
				s.mu.Lock()
				if latest, e2 := loadConversation(s.dataDir, convID); e2 == nil && latest.Steer != nil {
					latest.Steer.InjectID = injectID
					_ = saveConversation(s.dataDir, latest, s.secretsFor(convID))
				}
				s.mu.Unlock()
			}
			continue
		}
		secrets := s.secretsFor(convID)
		s.mu.Lock()
		if latest, e2 := loadConversation(s.dataDir, convID); e2 == nil {
			latest.Steer = nil
			latest.appendMessage("system", kindNotice, how, latest.CurrentSessionID, "steered", secrets)
			_ = saveConversation(s.dataDir, latest, secrets)
		}
		s.mu.Unlock()
		return
	}
}

func (s *Server) handleCancelSteer(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	s.stopSteerWaitLocked(c.ID)
	latest, err := loadConversation(s.dataDir, c.ID)
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	if sid := latest.CurrentSessionID; sid != "" {
		if h := s.mgr.Get(sid); h != nil {
			h.ClearPendingInject()
		}
	}
	if latest.Steer != nil {
		latest.Steer = nil
		latest.appendMessage("system", kindNotice, "已取消尚未注入的当前轮调整。", "", "", secrets)
		if err := saveConversation(s.dataDir, latest, secrets); err != nil {
			s.mu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.viewConversation(latest))
}

func (s *Server) handleCancelQueue(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	secrets := s.secretsFor(c.ID)
	s.mu.Lock()
	latest, err := loadConversation(s.dataDir, c.ID)
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	if latest.Queue != nil {
		latest.Queue = nil
		latest.appendMessage("system", kindNotice, "已取消排队中的下一条需求。", "", "", secrets)
		if err := saveConversation(s.dataDir, latest, secrets); err != nil {
			s.mu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.viewConversation(latest))
}

func (s *Server) handleConversationFiles(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	q := r.URL.Query().Get("q")
	limit := 30
	if n := queryInt(r, "limit", 30); n > 0 {
		limit = n
	}
	entries, err := searchProjectFiles(c.WorkDir, q, limit)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "query": q})
}

func (s *Server) handleTurnChanges(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConv(r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusForConvErr(err), map[string]string{"error": err.Error()})
		return
	}
	sid := r.PathValue("sessionId")
	if !containsString(c.SessionIDs, sid) && c.CurrentSessionID != sid {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "turn not found in this conversation"})
		return
	}
	changes, err := loadTurnChanges(s.dataDir, c.ID, sid)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "changes": []turnChangeView{}, "editable": false})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	ok, reason := s.turnChangesEditable(c, sid)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"changes":    viewTurnChanges(changes, ok, reason),
		"editable":   ok,
		"reason":     reason,
	})
}

type convError struct {
	status int
	msg    string
}

func (e *convError) Error() string { return e.msg }

func errConvConflict(msg string) error { return &convError{status: http.StatusConflict, msg: msg} }

// writeConvConflict returns a conflict (or other conv error) that still
// includes the conversation view, so a pending rollback is visible in the
// UI instead of a bare error payload.
func (s *Server) writeConvConflict(w http.ResponseWriter, c *Conversation, err error) {
	if c != nil {
		if latest, e2 := loadConversation(s.dataDir, c.ID); e2 == nil {
			c = latest
		}
	}
	body := map[string]any{"error": err.Error()}
	if c != nil {
		body["conversation"] = s.viewConversation(c)
	}
	writeJSON(w, statusForConvErr(err), body)
}
func errConvNeedKey(msg string) error {
	return &convError{status: http.StatusPreconditionRequired, msg: msg}
}
func errConvNotFound() error {
	return &convError{status: http.StatusNotFound, msg: "conversation not found"}
}

func statusForConvErr(err error) int {
	var ce *convError
	if errors.As(err, &ce) {
		return ce.status
	}
	switch {
	case errors.Is(err, agent.ErrWorkdirBusy),
		errors.Is(err, agent.ErrSessionActive),
		errors.Is(err, agent.ErrRunFinished),
		errors.Is(err, agent.ErrWrongPhase):
		return http.StatusConflict
	case os.IsNotExist(err):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func conversationDirectPath(dataDir, convID string) string {
	return filepath.Join(conversationsDir(dataDir), convID+".direct.json")
}

func conversationAgentMode(c *Conversation) string {
	if c == nil {
		return agent.AgentLoopGuided
	}
	return normalizeAgentMode(c.Settings.AgentMode)
}

func normalizeAgentMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), agent.AgentLoopDirect) {
		return agent.AgentLoopDirect
	}
	if strings.TrimSpace(mode) == "" {
		return ""
	}
	return agent.AgentLoopGuided
}

func startTurnNotice(c *Conversation) string {
	if conversationAgentMode(c) == agent.AgentLoopDirect {
		return "已开始直接 agent 循环：会实际读/搜索代码、修改、执行验证，并根据真实工具结果自修；结束时给出模型自己写的总结（不会先走任务树规划，也不会把状态日志当作回答）。"
	}
	return "已开始执行本轮需求（规划后会进入任务树；需要审批时会停在计划和 diff 审查）。"
}

func pendingSteerNotice(c *Conversation) string {
	if conversationAgentMode(c) == agent.AgentLoopDirect {
		return "已记下这条调整。将在当前工具结束后的下一模型步骤注入同一循环（不会取消正在执行的工具，也不会丢掉本轮已完成的修改）。可以用「取消调整」撤回。"
	}
	return "已记下这条调整。当前正处在计划审查之外的审批、问答或暂停等不能立刻打断的阶段，会在该步骤结束后注入当前轮（不会绕过审批，已完成工作保留）。执行中则会马上打断当前叶子。可以用「取消调整」撤回。"
}

// resumeDirectAndInject continues an unfinished direct loop and queues the
// new user text at the next model/tool boundary.
func (s *Server) resumeDirectAndInject(c *Conversation, userText, requestKey string, paths []string, sels []codeSelection) error {
	cfg, err := s.configForConversation(c)
	if err != nil {
		return err
	}
	if requestKey != "" {
		cfg.APIKey = requestKey
	}
	cfg.SessionID = c.CurrentSessionID
	cfg.Goal = ""
	cfg.WorkDir = c.WorkDir
	cfg.HoldServe = true
	client := s.newClient(cfg)
	h, err := s.mgr.Resume(cfg, client, agent.SilentLogger())
	if err != nil {
		return err
	}
	s.bindSession(c.ID, h.SessionID())
	s.armConversationFinalize(c.ID, h)
	secrets := s.secretsFor(c.ID)
	instruction := Redact(steerInstruction(c.WorkDir, userText, paths, sels), secrets...)
	injectID, ierr := h.InjectAtBoundaryID(instruction)
	if ierr != nil {
		_ = h.Abort()
		return ierr
	}
	s.mu.Lock()
	latest, loadErr := loadConversation(s.dataDir, c.ID)
	if loadErr == nil {
		*c = *latest
	}
	c.appendMessage("user", kindUser, userText, h.SessionID(), "steered", secrets)
	c.Messages[len(c.Messages)-1].Attachments = mergeFileRefs(refsFromPaths(paths), refsFromSelections(sels))
	c.Steer = &PendingSteer{Text: userText, Paths: paths, Selections: sels, InjectID: injectID, CreatedAt: time.Now().UTC()}
	c.CurrentSessionID = h.SessionID()
	c.appendMessage("system", kindNotice, pendingSteerNotice(c), h.SessionID(), "steer_pending", secrets)
	saveErr := saveConversation(s.dataDir, c, secrets)
	done := make(chan struct{})
	s.watchDone[c.ID] = done
	s.mu.Unlock()
	if saveErr != nil {
		_ = h.Abort()
		return saveErr
	}
	go s.watchConversationRun(c.ID, h, done)
	h.Launch()
	return nil
}

func (s *Server) forwardDirectEvents(convID string, evs <-chan map[string]any) {
	for ev := range evs {
		kind, _ := ev["kind"].(string)
		secrets := s.secretsFor(convID)
		s.mu.Lock()
		c, err := loadConversation(s.dataDir, convID)
		if err != nil {
			s.mu.Unlock()
			continue
		}
		sid := c.CurrentSessionID
		switch kind {
		case "tool_start":
			c.appendMessage("system", kindToolActivity, formatToolStart(ev), sid, evString(ev["tool"]), secrets)
			_ = saveConversation(s.dataDir, c, secrets)
		case "tool_call":
			c.appendMessage("system", kindToolActivity, formatToolActivity(ev), sid, evString(ev["tool"]), secrets)
			_ = saveConversation(s.dataDir, c, secrets)
		case "ask_pending":
			q := evString(ev["question"])
			if q != "" && !lastAskIs(c, q) {
				c.appendMessage("assistant", kindAsk, q, sid, "ask", secrets)
				_ = saveConversation(s.dataDir, c, secrets)
			}
		case "ask":
			ans := evString(ev["answer"])
			if ans != "" {
				c.appendMessage("user", kindAskAnswer, ans, sid, "ask_answer", secrets)
				_ = saveConversation(s.dataDir, c, secrets)
			}
		case "ask_timeout":
			text := "提问已超时未答。"
			if evString(ev["risk"]) == "consent" {
				text = "提问已超时，未作同意；已暂停。审批、破坏性或对外发送的操作不会因此自动执行。"
			} else {
				text = "提问已超时未答（这不是同意）。将按合理默认继续，不会把超时当成对审批或破坏性操作的批准。"
			}
			c.appendMessage("system", kindNotice, text, sid, "ask_timeout", secrets)
			_ = saveConversation(s.dataDir, c, secrets)
		case "leaf_approval":
			switch evString(ev["phase"]) {
			case "pending":
				n := ev["files"]
				text := "本轮改动正在等待你批准，才会写入项目工作区。"
				if n != nil {
					text = "本轮有改动正在等待你批准，才会写入项目工作区。"
				}
				c.appendMessage("system", kindNotice, text, sid, "approval_pending", secrets)
			case "approved":
				c.appendMessage("system", kindNotice, "本轮改动已批准并写入工作区。", sid, "approval_approved", secrets)
			case "rejected":
				text := "本轮改动已被拒绝，未写入工作区，不会自动重试绕过。"
				if cmt := evString(ev["comment"]); cmt != "" {
					text += " 原因：" + oneLine(cmt, 200)
				}
				c.appendMessage("system", kindNotice, text, sid, "approval_rejected", secrets)
			case "blocked":
				c.appendMessage("system", kindNotice, "未写入工作区：检测到工作区在审查期间被外部修改，为避免覆盖未合并。", sid, "approval_blocked", secrets)
			}
			_ = saveConversation(s.dataDir, c, secrets)
		case "user_inject":
			if matchesInjectAck(c.Steer, ev) {
				c.Steer = nil
			}
			text := "调整已在下一模型步骤注入当前循环，已完成的修改保留。"
			if inst := evString(ev["instruction"]); inst != "" {
				text = "调整已在下一模型步骤注入当前循环，已完成的修改保留。内容：" + oneLine(inst, 200)
			}
			c.appendMessage("system", kindNotice, text, sid, "inject_applied", secrets)
			_ = saveConversation(s.dataDir, c, secrets)
		}
		s.mu.Unlock()
	}
}

// matchesInjectAck reports whether a user_inject event corresponds to the
// currently persisted pending steer. A stale acknowledgement without a
// matching request id (or, if both lack ids, matching text) must not clear
// a newer instruction.
func matchesInjectAck(st *PendingSteer, ev map[string]any) bool {
	if st == nil {
		return false
	}
	id := evString(ev["inject_id"])
	if st.InjectID != "" || id != "" {
		return st.InjectID != "" && st.InjectID == id
	}
	inst := evString(ev["instruction"])
	return inst != "" && inst == st.Text
}

func lastAskIs(c *Conversation, question string) bool {
	if c == nil || len(c.Messages) == 0 {
		return false
	}
	last := c.Messages[len(c.Messages)-1]
	return last.Kind == kindAsk && last.Text == question
}

func formatToolStart(ev map[string]any) string {
	tool := evString(ev["tool"])
	if tool == "" {
		tool = "tool"
	}
	line := "开始 " + tool
	if args := oneLine(evString(ev["args"]), 160); args != "" {
		line += " " + args
	}
	return line
}

func formatToolActivity(ev map[string]any) string {
	tool := evString(ev["tool"])
	if tool == "" {
		tool = "tool"
	}
	ok := ev["ok"]
	status := "ok"
	if ok == false {
		status = "error"
	}
	args := oneLine(evString(ev["args"]), 160)
	preview := strings.TrimSpace(evString(ev["output_preview"]))
	if errText := evString(ev["error"]); errText != "" && preview == "" {
		preview = errText
	}
	line := tool + " " + status
	if args != "" {
		line += " " + args
	}
	if preview != "" {
		line += "\n" + oneLine(preview, 400)
	}
	return line
}

func evString(v any) string {
	s, _ := v.(string)
	return s
}
