package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
)

// Conversation is a persistent project thread: one workdir, saved run
// settings (never the API key), a message log, and a short work log used
// as the only earlier-turn context for follow-up runs.
type Conversation struct {
	ID               string               `json:"id"`
	Title            string               `json:"title"`
	WorkDir          string               `json:"workdir"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
	Settings         ConversationSettings `json:"settings"`
	Messages         []ConvMessage        `json:"messages"`
	WorkLog          []string             `json:"work_log"`
	SessionIDs       []string             `json:"session_ids"`
	CurrentSessionID string               `json:"current_session_id,omitempty"`
	Queue            *QueuedTurn          `json:"queue,omitempty"`
	Steer            *PendingSteer        `json:"steer,omitempty"`
	RolledBackTurns  []string             `json:"rolled_back_turns,omitempty"`
}

// QueuedTurn is the single follow-up waiting to start when the current run
// finishes (completed, failed, or aborted — not paused).
type QueuedTurn struct {
	Text       string          `json:"text"`
	Paths      []string        `json:"paths,omitempty"`
	Selections []codeSelection `json:"selections,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// PendingSteer is a mid-run follow-up that should be injected into the
// current turn (next leaf or a replan) rather than starting a new turn.
type PendingSteer struct {
	Text       string          `json:"text"`
	Paths      []string        `json:"paths,omitempty"`
	Selections []codeSelection `json:"selections,omitempty"`
	InjectID   string          `json:"inject_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ConversationSettings are the knobs reused on every turn in a conversation.
// API keys must never appear here or in the conversation file.
type ConversationSettings struct {
	Model        string  `json:"model,omitempty"`
	BaseURL      string  `json:"base_url,omitempty"`
	Parallel     int     `json:"parallel,omitempty"`
	Isolate      bool    `json:"isolate"`
	GitCommit    bool    `json:"git_commit"`
	Web          bool    `json:"web"`
	Browser      bool    `json:"browser"`
	ApprovalMode string  `json:"approval_mode,omitempty"`
	BudgetTokens int     `json:"budget_tokens,omitempty"`
	MaxCost      float64 `json:"max_cost,omitempty"`
	// AgentMode is "direct" (persistent tool loop) or "guided" (task tree).
	// Empty is treated as guided so older conversation files stay compatible.
	AgentMode string `json:"agent_mode,omitempty"`
}

// ConvMessage is one visible item in the conversation transcript.
// Kind distinguishes user requests from execution status; raw logs are
// never stored as if they were a model reply.
type ConvMessage struct {
	ID          string              `json:"id"`
	Role        string              `json:"role"` // "user", "system", or "assistant"
	Kind        string              `json:"kind"` // user, run_started, run_finished, notice, need_key
	Text        string              `json:"text"`
	SessionID   string              `json:"session_id,omitempty"`
	Outcome     string              `json:"outcome,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	Attachments []fileRef           `json:"attachments,omitempty"`
	Changes     []turnChangeSummary `json:"changes,omitempty"`
}

const (
	convMaxWorkLog     = 24
	memRecentTurns     = 4
	memRecentUserChars = 2000
	memRecentResChars  = 800
	memOldTurns        = 16
	memMaxBytes        = 24 << 10

	// convContextNote is shown to the user and stored on every API view so
	// we never imply a raw-chat memory the engine does not have.
	convContextNote = "同一对话会沿用本项目目录和已保存的模型设置。直接 agent 模式在同一段工具对话里读/搜索/修改/按真实工具结果自修，并给出模型自己写的总结；任务树模式仍会先规划再按叶子执行。近期约 4 轮会保留较完整的需求、插入的调整和变更要点；更早的轮次会压缩成一行摘要，总共大约二十轮、体积有上限——不是无限完整记忆，工作区里的文件才是事实来源。项目感知来自实际的读/检索和一份有限的目录地图，不是语义索引；用 @ 仍可强制附加指定文件（有大小和路径限制，不会整库原文注入）。运行中默认把追问「插入到当前轮」：直接模式下会在当前工具结束后的下一模型步骤注入同一循环，不会取消丢掉整轮已完成的修改；任务树模式下执行中会立刻打断当前叶子并纳入接下来的步骤或重新规划（已完成叶子保留，该叶子尚未合并的改动会丢弃）。若正在叶子审批或问答则先记下，不会绕过审批。也可以改排队到下一轮，或中止并用新指令接管。暂停后请先续跑，或中止后接管；暂停期间可以把调整记下，续跑后再注入，不会自动开跑排队项。API key 只留在本次服务进程的内存中，不会写入对话历史或事件流。服务重启后若模型需要密钥，需要重新填写。"

	kindUser         = "user"
	kindRunStarted   = "run_started"
	kindRunFinished  = "run_finished"
	kindNotice       = "notice"
	kindNeedKey      = "need_key"
	kindModelReply   = "model_reply"
	kindToolActivity = "tool_activity"
	kindAsk          = "ask"
	kindAskAnswer    = "ask_answer"
)

func conversationsDir(dataDir string) string {
	return filepath.Join(dataDir, "conversations")
}

func conversationPath(dataDir, id string) string {
	return filepath.Join(conversationsDir(dataDir), id+".json")
}

func validConversationID(id string) bool {
	if !strings.HasPrefix(id, "conv_") {
		return false
	}
	if len(id) < 10 || len(id) > 80 {
		return false
	}
	for _, r := range id[5:] {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func newConvID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "conv_" + time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:]), nil
}

func newSessionID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "sess_" + time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

func newMsgID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "msg_" + time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

func newConversation(workdir string, settings ConversationSettings, title string) (*Conversation, error) {
	id, err := newConvID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(title) == "" {
		title = filepath.Base(workdir)
	}
	return &Conversation{
		ID:        id,
		Title:     title,
		WorkDir:   workdir,
		CreatedAt: now,
		UpdatedAt: now,
		Settings:  settings,
		Messages:  nil,
		WorkLog:   nil,
	}, nil
}

func (c *Conversation) appendMessage(role, kind, text, sessionID, outcome string, secrets []string) ConvMessage {
	msg := ConvMessage{
		ID:        newMsgID(),
		Role:      role,
		Kind:      kind,
		Text:      Redact(text, secrets...),
		SessionID: sessionID,
		Outcome:   outcome,
		CreatedAt: time.Now().UTC(),
	}
	c.Messages = append(c.Messages, msg)
	c.UpdatedAt = msg.CreatedAt
	return msg
}

func (c *Conversation) appendWorkLog(entry string, secrets []string) {
	entry = strings.TrimSpace(Redact(entry, secrets...))
	if entry == "" {
		return
	}
	c.WorkLog = append(c.WorkLog, entry)
	if len(c.WorkLog) > convMaxWorkLog {
		c.WorkLog = append([]string(nil), c.WorkLog[len(c.WorkLog)-convMaxWorkLog:]...)
	}
}

func saveConversation(dataDir string, c *Conversation, secrets []string) error {
	if c == nil {
		return fmt.Errorf("cannot save nil conversation")
	}
	if !validConversationID(c.ID) {
		return fmt.Errorf("invalid conversation id")
	}
	// Belt-and-braces: never let a known key land on disk, even if a user
	// pasted it into a request or a summary echoed it.
	c.Title = Redact(c.Title, secrets...)
	for i := range c.Messages {
		c.Messages[i].Text = Redact(c.Messages[i].Text, secrets...)
	}
	for i := range c.WorkLog {
		c.WorkLog[i] = Redact(c.WorkLog[i], secrets...)
	}
	if c.Steer != nil {
		c.Steer.Text = Redact(c.Steer.Text, secrets...)
	}
	if c.Queue != nil {
		c.Queue.Text = Redact(c.Queue.Text, secrets...)
	}
	c.UpdatedAt = time.Now().UTC()

	dir := conversationsDir(dataDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create conversations dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode conversation: %w", err)
	}
	// Final scan of the raw payload so a future field cannot leak a key.
	payload := RedactBytes(data, secrets...)

	tmp, err := os.CreateTemp(dir, "conv_*.tmp")
	if err != nil {
		return fmt.Errorf("create temp conversation file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write conversation: %w", err)
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
	if err := os.Rename(tmpPath, conversationPath(dataDir, c.ID)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replace conversation file: %w", err)
	}
	return nil
}

func loadConversation(dataDir, id string) (*Conversation, error) {
	if !validConversationID(id) {
		return nil, fmt.Errorf("invalid conversation id")
	}
	data, err := os.ReadFile(conversationPath(dataDir, id))
	if err != nil {
		return nil, err
	}
	var c Conversation
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse conversation %s: %w", id, err)
	}
	if c.ID != id {
		return nil, fmt.Errorf("conversation id mismatch")
	}
	return &c, nil
}

func listConversations(dataDir string) ([]*Conversation, error) {
	dir := conversationsDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Conversation, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		c, err := loadConversation(dataDir, id)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func deleteConversationFile(dataDir, id string) error {
	if !validConversationID(id) {
		return fmt.Errorf("invalid conversation id")
	}
	err := os.Remove(conversationPath(dataDir, id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// followUpGoal builds the goal sent to a new guided run. The user's current
// request comes first; earlier conversation memory and automatic project
// context follow, then any explicit @ attachments.
func followUpGoal(userText, memory, fileContext, projectContext string) string {
	userText = strings.TrimSpace(userText)
	var b strings.Builder
	b.WriteString(userText)
	if strings.TrimSpace(memory) != "" {
		b.WriteString("\n\n---\nConversation memory (recent turns kept in more detail, earlier turns compressed; not infinite raw chat; files on disk are the source of truth):\n")
		b.WriteString(strings.TrimSpace(memory))
		b.WriteByte('\n')
	}
	if strings.TrimSpace(projectContext) != "" {
		b.WriteString(projectContext)
	}
	if strings.TrimSpace(fileContext) != "" {
		b.WriteString(fileContext)
	}
	return strings.TrimSpace(b.String())
}

// conversationMemory prefers recent user requests, mid-turn steers and change
// lists in fuller form, and compresses older turns.
func conversationMemory(c *Conversation) string {
	if c == nil {
		return ""
	}
	turns := groupConvTurns(c)
	if len(turns) == 0 {
		// Fall back to the older work-log shape if messages are missing.
		if len(c.WorkLog) == 0 {
			return ""
		}
		var b strings.Builder
		for i, e := range c.WorkLog {
			fmt.Fprintf(&b, "%d. %s\n", i+1, e)
		}
		return strings.TrimSpace(b.String())
	}
	recentFrom := 0
	if len(turns) > memRecentTurns {
		recentFrom = len(turns) - memRecentTurns
	}
	oldFrom := 0
	if recentFrom > memOldTurns {
		oldFrom = recentFrom - memOldTurns
	}
	var b strings.Builder
	if len(c.RolledBackTurns) > 0 {
		b.WriteString("IMPORTANT: one or more earlier turns were rolled back. Their file changes were undone and are no longer on disk. Do not assume those edits are still in the project; the workspace is the source of truth.\n")
	}
	if oldFrom > 0 {
		fmt.Fprintf(&b, "(omitted %d older turns)\n", oldFrom)
	}
	n := 1
	for i := oldFrom; i < recentFrom; i++ {
		fmt.Fprintf(&b, "%d. %s\n", n, turns[i].compact())
		n++
	}
	if recentFrom < len(turns) {
		b.WriteString("Recent turns in more detail:\n")
		for i := recentFrom; i < len(turns); i++ {
			b.WriteString(turns[i].detailed())
		}
	}
	out := b.String()
	if len(out) > memMaxBytes {
		return out[:memMaxBytes] + "\n...(conversation memory truncated)\n"
	}
	return strings.TrimSpace(out)
}

type convTurn struct {
	user        string
	sessionID   string
	steers      []string
	outcome     string
	result      string
	changes     []string
	attachments []string
	rolledBack  bool
}

func (t convTurn) compact() string {
	var b strings.Builder
	fmt.Fprintf(&b, "User: %s", oneLine(t.user, 200))
	if t.outcome != "" {
		fmt.Fprintf(&b, " | Outcome: %s", t.outcome)
	}
	if len(t.changes) > 0 {
		fmt.Fprintf(&b, " | Changes: %s", strings.Join(t.changes, ", "))
	}
	if s := oneLine(t.result, 160); s != "" {
		fmt.Fprintf(&b, " | Result: %s", s)
	}
	if t.rolledBack {
		b.WriteString(" | ROLLED BACK: these file changes were undone and no longer exist on disk. Do not assume they are still in the project.")
	}
	return b.String()
}

func (t convTurn) detailed() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- User: %s\n", clipRunes(strings.TrimSpace(t.user), memRecentUserChars))
	if len(t.attachments) > 0 {
		fmt.Fprintf(&b, "  Attachments: %s\n", strings.Join(t.attachments, ", "))
	}
	for _, s := range t.steers {
		fmt.Fprintf(&b, "  Injected follow-up: %s\n", clipRunes(s, 400))
	}
	if t.outcome != "" {
		fmt.Fprintf(&b, "  Outcome: %s\n", t.outcome)
	}
	if len(t.changes) > 0 {
		fmt.Fprintf(&b, "  Changes: %s\n", strings.Join(t.changes, ", "))
	}
	if s := clipRunes(strings.TrimSpace(t.result), memRecentResChars); s != "" {
		fmt.Fprintf(&b, "  Result: %s\n", s)
	}
	if t.rolledBack {
		b.WriteString("  ROLLED BACK: these file changes were undone and no longer exist on disk. Do not assume they are still in the project.\n")
	}
	return b.String()
}

func groupConvTurns(c *Conversation) []convTurn {
	var turns []convTurn
	var cur *convTurn
	flush := func() {
		if cur != nil && (strings.TrimSpace(cur.user) != "" || cur.outcome != "") {
			turns = append(turns, *cur)
		}
		cur = nil
	}
	for _, m := range c.Messages {
		switch m.Kind {
		case kindUser:
			// A user message mid-run (has session id of a started turn and a
			// current turn already open) is a steer, not a new turn.
			if cur != nil && m.SessionID != "" && cur.outcome == "" && strings.TrimSpace(cur.user) != "" {
				cur.steers = append(cur.steers, m.Text)
				continue
			}
			flush()
			cur = &convTurn{user: m.Text, sessionID: m.SessionID}
			for _, a := range m.Attachments {
				if a.Path != "" {
					cur.attachments = append(cur.attachments, a.Path)
				}
			}
		case kindRunFinished:
			if cur == nil {
				cur = &convTurn{}
			}
			cur.outcome = m.Outcome
			cur.result = m.Text
			if m.SessionID != "" {
				cur.sessionID = m.SessionID
			}
			for _, ch := range m.Changes {
				cur.changes = append(cur.changes, ch.Status+":"+ch.Path)
			}
			flush()
		case kindNotice:
			if m.Outcome == "turn_rolled_back" && m.SessionID != "" {
				for i := range turns {
					if turns[i].sessionID == m.SessionID {
						turns[i].rolledBack = true
					}
				}
				if cur != nil && cur.sessionID == m.SessionID {
					cur.rolledBack = true
				}
			}
		}
	}
	flush()
	return turns
}

func clipRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// compactTurn summarizes one finished run for the work log.
func compactTurn(userText, outcome, result string, tree *engine.TaskTree) string {
	var b strings.Builder
	fmt.Fprintf(&b, "User: %s", oneLine(userText, 200))
	if outcome != "" {
		fmt.Fprintf(&b, " | Outcome: %s", outcome)
	}
	if tree != nil {
		done, all, _ := tree.GetLeafProgress()
		fmt.Fprintf(&b, " | Leaves: %d/%d", done, all)
		if root := tree.GetRoot(); root != nil {
			if root.ResultSummary != "" {
				result = root.ResultSummary
			} else if root.ErrorMsg != "" && result == "" {
				result = root.ErrorMsg
			}
		}
	}
	if s := oneLine(result, 240); s != "" {
		fmt.Fprintf(&b, " | Result: %s", s)
	}
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n > 0 && len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func lastUserText(c *Conversation, sessionID string) string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		m := c.Messages[i]
		if m.Kind != kindUser {
			continue
		}
		if sessionID == "" || m.SessionID == sessionID {
			return m.Text
		}
	}
	return ""
}

func runOutcome(phase string, failed bool) string {
	switch phase {
	case "done":
		return "completed"
	case "paused":
		return "paused"
	case "failed":
		if failed {
			return "failed"
		}
		return "failed"
	default:
		if phase != "" {
			return phase
		}
		return "ended"
	}
}

func finishedMessageText(outcome, result string, tree *engine.TaskTree) string {
	label := map[string]string{
		"completed": "执行结束：已完成",
		"paused":    "执行已暂停，可在本对话续跑，或中止后再发新需求",
		"failed":    "执行结束：失败",
		"aborted":   "执行已中止",
	}[outcome]
	if label == "" {
		label = "执行结束：" + outcome
	}
	var extra []string
	if tree != nil {
		done, all, _ := tree.GetLeafProgress()
		if all > 0 {
			extra = append(extra, fmt.Sprintf("叶子 %d/%d", done, all))
		}
		if root := tree.GetRoot(); root != nil && root.State == models.TaskStateFailed && root.ErrorMsg != "" && result == "" {
			result = root.ErrorMsg
		}
	}
	if r := oneLine(result, 300); r != "" {
		extra = append(extra, r)
	}
	if len(extra) == 0 {
		return label + "。"
	}
	return label + "。" + strings.Join(extra, "；")
}
