package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteServer covers the Streamable-HTTP transport: JSON responses, SSE
// responses that also carry unrelated notifications, session continuity, and
// configured headers. Remote servers are how hosted MCP endpoints are exposed;
// without this they would fail the "command or url" validation.
func TestRemoteServer(t *testing.T) {
	var sawSession, sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		sawAuth = true
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := msg["id"]
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-42")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "remote", "version": "1"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") == "session-42" {
				sawSession = true
			}
			// SSE response: an unrelated notification first, then the result.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, "event: message")
			fmt.Fprintf(w, "data: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%v,"result":{"tools":[{"name":"remote_echo","description":"Echo remotely.","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}}`,
				id))
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "session-42" {
				http.Error(w, "lost session", http.StatusNotFound)
				return
			}
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			raw, _ := json.Marshal(msg["params"])
			_ = json.Unmarshal(raw, &params)
			text, _ := params.Arguments["text"].(string)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "remote: " + text}}},
			})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rt, err := Start(context.Background(), map[string]ServerConfig{
		"remote": {URL: srv.URL, Headers: map[string]string{"X-Api-Key": "secret"}},
	}, t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("start remote: %v", err)
	}
	defer rt.Close()
	if !sawAuth || !sawSession {
		t.Fatalf("server did not receive configured auth/session headers: auth=%v session=%v", sawAuth, sawSession)
	}
	if !rt.HasTool("mcp__remote__remote_echo") {
		t.Fatalf("remote tool missing: %+v", rt.tools)
	}
	out, err := rt.Call(context.Background(), "mcp__remote__remote_echo", map[string]any{"text": "hi"})
	if err != nil || out != "remote: hi" {
		t.Fatalf("remote call = %q, %v", out, err)
	}
}

func TestLoadConfigTransportValidation(t *testing.T) {
	data := t.TempDir()
	writeJSON(t, filepath.Join(data, "mcp.json"), `{"mcpServers":{"both":{"command":"x","url":"https://example.invalid"}}}`)
	if _, err := LoadConfig(data, ""); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("both transports should be rejected: %v", err)
	}
	writeJSON(t, filepath.Join(data, "mcp.json"), `{"mcpServers":{"neither":{}}}`)
	if _, err := LoadConfig(data, ""); err == nil {
		t.Fatal("server with neither command nor url should be rejected")
	}
	writeJSON(t, filepath.Join(data, "mcp.json"), `{"mcpServers":{"remote":{"url":"https://example.invalid/mcp","headers":{"Authorization":"Bearer x"}}}}`)
	cfg, err := LoadConfig(data, "")
	if err != nil || cfg["remote"].Headers["Authorization"] != "Bearer x" {
		t.Fatalf("remote config should load: %+v err=%v", cfg, err)
	}
}

// TestRemoteSSEToolErrorIsSurfaced: a remote MCP server that answers a tool
// call with a JSON-RPC error must reach the caller as an error.
//
// The SSE path used to fabricate a result for it, and the caller decoded that
// as "the tool returned no text content" — the actual reason for the failure
// was lost, and the model was told the tool had nothing to say.
func TestRemoteSSEToolErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := msg["id"]
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "remote", "version": "1"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%v,"result":{"tools":[{"name":"remote_fail","inputSchema":{"type":"object","properties":{}}}]}}`,
				id))
		case "tools/call":
			// The tool fails, over SSE.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%v,"error":{"code":-32000,"message":"tool exploded"}}`,
				id))
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rt, err := Start(context.Background(), map[string]ServerConfig{
		"remote": {URL: srv.URL},
	}, t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("start remote: %v", err)
	}
	defer rt.Close()

	_, err = rt.Call(context.Background(), "mcp__remote__remote_fail", map[string]any{})
	if err == nil {
		t.Fatal("a failing remote tool must return an error")
	}
	if !strings.Contains(err.Error(), "tool exploded") {
		t.Fatalf("the error message was lost: %v", err)
	}
}
