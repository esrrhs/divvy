package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/esrrhs/divvy/pkg/llm"
)

const (
	initTimeout   = 20 * time.Second
	callTimeout   = 3 * time.Minute
	closeTimeout  = 3 * time.Second
	maxReadBytes  = 32 << 20
	maxOutputSize = 64 << 10
	maxListPages  = 10
)

// Runtime owns the live stdio MCP servers for one agent turn and the flattened
// tool index the model sees. Servers are opt-in through mcp.json; a broken
// server is skipped rather than failing the whole run.
type Runtime struct {
	servers []*server
	tools   map[string]toolRef
	defs    []llm.Tool
	guide   string
}

type toolRef struct {
	server *server
	name   string
}

type server struct {
	name string
	// cmd/in/out are set for stdio servers; url/http/headers for remote ones.
	cmd        *exec.Cmd
	in         io.WriteCloser
	out        *bufio.Reader
	err        *limitedBuffer
	url        string
	httpClient *http.Client
	headers    map[string]string
	sessionID  string

	wmu     sync.Mutex
	mu      sync.Mutex
	sessMu  sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage
	done    chan struct{}
	once    sync.Once
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type listResult struct {
	Tools      []toolInfo `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

type callContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
}

type callResult struct {
	Content           []callContent `json:"content"`
	IsError           bool          `json:"isError"`
	StructuredContent any           `json:"structuredContent"`
}

// Start launches every enabled server in cfg, initializes it, and indexes its
// tools. It returns a Runtime even when some servers failed (the healthy ones
// remain usable) plus a joined error describing the failures.
func Start(ctx context.Context, cfg map[string]ServerConfig, workdir string, logf func(string, ...any)) (*Runtime, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	rt := &Runtime{tools: map[string]toolRef{}}
	var failures []string
	for _, name := range ServerNames(cfg) {
		serverCfg := cfg[name]
		s, err := startServer(ctx, name, serverCfg, workdir)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			logf("mcp server %s unavailable: %v", name, err)
			continue
		}
		if err := s.initialize(ctx); err != nil {
			s.Close()
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			logf("mcp server %s failed to initialize: %v", name, err)
			continue
		}
		rt.servers = append(rt.servers, s)
	}
	if len(rt.servers) == 0 {
		if len(failures) > 0 {
			return nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, nil
	}
	for _, s := range rt.servers {
		if err := rt.indexServer(ctx, s); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", s.name, err))
			logf("mcp server %s tool listing failed: %v", s.name, err)
		}
	}
	rt.guide = renderGuide(rt)
	if len(failures) > 0 {
		return rt, errors.New(strings.Join(failures, "; "))
	}
	return rt, nil
}

func startServer(ctx context.Context, name string, cfg ServerConfig, workdir string) (*server, error) {
	if strings.TrimSpace(cfg.URL) != "" {
		return startRemote(name, cfg), nil
	}
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	buf := &limitedBuffer{max: 16 << 10}
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{
		name:    name,
		cmd:     cmd,
		in:      in,
		out:     bufio.NewReaderSize(stdout, 64<<10),
		err:     buf,
		pending: map[int64]chan rpcMessage{},
		done:    make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

func startRemote(name string, cfg ServerConfig) *server {
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	return &server{
		name:       name,
		url:        strings.TrimSpace(cfg.URL),
		httpClient: &http.Client{Timeout: callTimeout},
		headers:    headers,
		pending:    map[int64]chan rpcMessage{},
		done:       make(chan struct{}),
	}
}

func (s *server) initialize(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "divvy", "version": "0.1"},
	}
	if _, err := s.request(initCtx, "initialize", params); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := s.notify("notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

func (s *server) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if s.url != "" {
		return s.httpRequest(ctx, method, params)
	}
	id := s.nextRequestID()
	ch := make(chan rpcMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := s.write(msg); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("%s: server exited (%s)", method, s.stderrTail())
	}
}

func (s *server) notify(method string, params any) error {
	if s.url != "" {
		// Notifications are fire-and-forget; still bound them so a dead
		// endpoint cannot hold the turn open.
		ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
		defer cancel()
		_, err := s.httpPost(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params}, 0)
		return err
	}
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *server) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write to mcp server %s: %w (stderr: %s)", s.name, err, s.stderrTail())
	}
	return nil
}

func (s *server) nextRequestID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

func (s *server) readLoop() {
	defer close(s.done)
	for {
		line, err := s.out.ReadBytes('\n')
		if len(line) > 0 {
			line = []byte(strings.TrimSpace(string(line)))
		}
		if len(line) > 0 {
			s.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				// Nothing reads stderr live; keep the tail for diagnostics.
				s.err.WriteString("read error: " + err.Error())
			}
			s.failPending(fmt.Errorf("mcp server %s closed", s.name))
			return
		}
		if len(line) >= maxReadBytes {
			s.failPending(fmt.Errorf("mcp server %s sent an oversized message", s.name))
			return
		}
	}
}

func (s *server) dispatch(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.Method != "" {
		// Server-to-client requests are not supported. Answer them with a
		// JSON-RPC method-not-found so a well-behaved server can continue
		// instead of waiting for a reply that never comes.
		if len(msg.ID) > 0 {
			_ = s.write(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error":   map[string]any{"code": -32601, "message": "method not supported by divvy"},
			})
		}
		return
	}
	if len(msg.ID) == 0 {
		return
	}
	id, err := strconv.ParseInt(strings.Trim(string(msg.ID), `"`), 10, 64)
	if err != nil {
		return
	}
	s.mu.Lock()
	ch := s.pending[id]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *server) failPending(err error) {
	s.mu.Lock()
	pending := s.pending
	s.pending = map[int64]chan rpcMessage{}
	s.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpcMessage{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

func (s *server) stderrTail() string {
	return strings.TrimSpace(s.err.String())
}

// Close speaks the stdio shutdown convention (close stdin) and then kills a
// server that did not exit promptly. MCP has no required shutdown request.
func (s *server) Close() {
	if s.url != "" {
		return
	}
	s.once.Do(func() {
		_ = s.in.Close()
		waited := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(closeTimeout):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-waited
		}
	})
}

// httpRequest performs one JSON-RPC request over Streamable HTTP. It accepts
// either a single JSON response or an SSE stream; unrelated notifications are
// skipped until the matching response id arrives.
func (s *server) httpRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextRequestID()
	raw, err := s.httpPost(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, id)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *server) httpPost(ctx context.Context, msg map[string]any, wantID int64) (json.RawMessage, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2024-11-05")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	s.sessMu.Lock()
	session := s.sessionID
	s.sessMu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http %s: %w", s.name, err)
	}
	defer resp.Body.Close()
	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		s.sessMu.Lock()
		s.sessionID = sid
		s.sessMu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("mcp http %s: status %d: %s", s.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if wantID == 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReadBytes))
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, wantID)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return nil, err
	}
	var rpc rpcMessage
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("MCP error: %s", rpc.Error.Message)
	}
	if len(rpc.Result) == 0 {
		return nil, fmt.Errorf("MCP response has no result")
	}
	return rpc.Result, nil
}

func readSSE(r io.Reader, wantID int64) (json.RawMessage, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	var data strings.Builder
	flush := func() (json.RawMessage, bool) {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil, false
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, false
		}
		if len(msg.ID) == 0 {
			return nil, false
		}
		id, err := strconv.ParseInt(strings.Trim(string(msg.ID), `"`), 10, 64)
		if err != nil || id != wantID {
			return nil, false
		}
		if msg.Error != nil {
			return json.RawMessage(`{"error":"` + msg.Error.Message + `"}`), true
		}
		return msg.Result, true
	}
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if result, ok := flush(); ok {
				return result, nil
			}
		case strings.HasPrefix(trimmed, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			data.WriteByte('\n')
		}
		if err != nil {
			if result, ok := flush(); ok {
				return result, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("MCP SSE stream ended before the response")
			}
			return nil, err
		}
	}
}

func (rt *Runtime) indexServer(ctx context.Context, s *server) error {
	listCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	cursor := ""
	seenCursor := map[string]bool{}
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := s.request(listCtx, "tools/list", params)
		if err != nil {
			return err
		}
		var result listResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode tools/list: %w", err)
		}
		for _, tool := range result.Tools {
			if strings.TrimSpace(tool.Name) == "" {
				continue
			}
			full := rt.uniqueToolName(s.name, tool.Name)
			rt.tools[full] = toolRef{server: s, name: tool.Name}
			desc := strings.TrimSpace(tool.Description)
			if desc == "" {
				desc = "MCP tool " + tool.Name + " from " + s.name
			}
			params := tool.InputSchema
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			rt.defs = append(rt.defs, llm.Tool{
				Type: "function",
				Function: llm.FunctionDef{
					Name:        full,
					Description: desc,
					Parameters:  params,
				},
			})
		}
		cursor = strings.TrimSpace(result.NextCursor)
		if cursor == "" || seenCursor[cursor] {
			break
		}
		seenCursor[cursor] = true
	}
	sort.SliceStable(rt.defs, func(i, j int) bool { return rt.defs[i].Function.Name < rt.defs[j].Function.Name })
	return nil
}

func (rt *Runtime) uniqueToolName(server, tool string) string {
	base := "mcp__" + safeName(server) + "__" + safeName(tool)
	if len(base) > 64 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(base))
		suffix := fmt.Sprintf("_%08x", h.Sum32())
		base = base[:64-len(suffix)] + suffix
	}
	name := base
	for i := 2; ; i++ {
		if _, exists := rt.tools[name]; !exists {
			return name
		}
		suffix := fmt.Sprintf("_%d", i)
		if len(base)+len(suffix) > 64 {
			name = base[:64-len(suffix)] + suffix
		} else {
			name = base + suffix
		}
	}
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// ToolDefs returns the native tool definitions for every discovered tool.
func (rt *Runtime) ToolDefs() []llm.Tool {
	if rt == nil {
		return nil
	}
	return rt.defs
}

// HasTool reports whether name belongs to an MCP server, so the caller can
// route it before the sandbox rejects an unknown tool.
func (rt *Runtime) HasTool(name string) bool {
	if rt == nil {
		return false
	}
	_, ok := rt.tools[name]
	return ok
}

// ServerCount reports how many servers initialized successfully.
func (rt *Runtime) ServerCount() int {
	if rt == nil {
		return 0
	}
	return len(rt.servers)
}

// Guide renders the JSON-action description of MCP tools. Native tool calling
// uses ToolDefs instead; both paths share the same names.
func (rt *Runtime) Guide() string {
	if rt == nil {
		return ""
	}
	return rt.guide
}

func renderGuide(rt *Runtime) string {
	if len(rt.defs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("MCP tools (external servers; arguments follow each tool's JSON schema):\n")
	names := make([]string, 0, len(rt.tools))
	for name := range rt.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := rt.tools[name]
		var schema struct {
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		}
		for _, def := range rt.defs {
			if def.Function.Name == name {
				schema.Description = def.Function.Description
				schema.Parameters = def.Function.Parameters
				break
			}
		}
		fmt.Fprintf(&b, "- %s: %s", name, clipText(schema.Description, 160))
		if args := describeSchema(schema.Parameters); args != "" {
			b.WriteString(" args: ")
			b.WriteString(args)
		}
		b.WriteString("\n(server: ")
		b.WriteString(ref.server.name)
		b.WriteString(", tool: ")
		b.WriteString(ref.name)
		b.WriteString(")\n")
	}
	return b.String()
}

func describeSchema(params map[string]any) string {
	if params == nil {
		return ""
	}
	props, _ := params["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		typ := "value"
		if p, ok := props[k].(map[string]any); ok {
			if t, ok := p["type"].(string); ok && t != "" {
				typ = t
			}
		}
		parts = append(parts, k+":"+typ)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Call executes one tool on its owning server. Output is flattened to text so
// it can enter a normal tool-result message; images and binary resources are
// described rather than embedded.
func (rt *Runtime) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	ref, ok := rt.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown MCP tool %q", name)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	raw, err := ref.server.request(callCtx, "tools/call", map[string]any{
		"name":      ref.name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var result callResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode MCP tool result: %w", err)
	}
	text := flattenResult(result)
	if result.IsError {
		if text == "" {
			text = "MCP tool reported an error without details"
		}
		return "", errors.New(text)
	}
	if text == "" {
		text = "(MCP tool returned no text content)"
	}
	return text, nil
}

func flattenResult(result callResult) string {
	var parts []string
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "image":
			parts = append(parts, fmt.Sprintf("[image content omitted: %s, %d base64 bytes]", c.MimeType, len(c.Data)))
		case "resource":
			switch {
			case c.URI != "":
				parts = append(parts, "[resource: "+c.URI+"]")
			case c.Name != "":
				parts = append(parts, "[resource: "+c.Name+"]")
			default:
				parts = append(parts, "[resource content omitted]")
			}
		default:
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	if len(parts) == 0 && result.StructuredContent != nil {
		if b, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, string(b))
		}
	}
	out := strings.Join(parts, "\n")
	if len(out) > maxOutputSize {
		out = out[:maxOutputSize] + fmt.Sprintf("\n[truncated to %d bytes]", maxOutputSize)
	}
	return out
}

// Close shuts down every server. Safe on a nil Runtime.
func (rt *Runtime) Close() {
	if rt == nil {
		return
	}
	for _, s := range rt.servers {
		s.Close()
	}
}

// limitedBuffer keeps only the tail of a child's stderr: enough for an error
// message, not enough for a logging server to exhaust memory.
type limitedBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = append([]byte(nil), b.data[len(b.data)-b.max:]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *limitedBuffer) WriteString(s string) {
	_, _ = b.Write([]byte(s))
}

func clipText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
