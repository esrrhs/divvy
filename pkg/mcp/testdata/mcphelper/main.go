// Command mcphelper is a tiny stdio MCP server used by divvy's tests. It
// implements just enough of the protocol to exercise initialization, tool
// listing (including pagination), calls, and tool errors.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mcphelper", "version": "1.0"},
			}})
		case "notifications/initialized":
			// notification: no reply
		case "tools/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Cursor == "page2" {
				_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"tools": []any{map[string]any{
						"name":        "fail",
						"description": "Always reports a tool error.",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"message": map[string]any{"type": "string"}},
							"required":   []string{"message"},
						},
					}},
				}})
				continue
			}
			_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"tools": []any{map[string]any{
					"name":        "echo",
					"description": "Echo back the supplied text.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"},
					},
				}},
				"nextCursor": "page2",
			}})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			text, _ := params.Arguments["text"].(string)
			message, _ := params.Arguments["message"].(string)
			switch params.Name {
			case "echo":
				_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "echo: " + text}},
				}})
			case "fail":
				_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"isError": true,
					"content": []any{map[string]any{"type": "text", "text": "boom: " + message}},
				}})
			default:
				_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{
					"code": -32602, "message": fmt.Sprintf("unknown tool %q", params.Name),
				}})
			}
		default:
			if len(req.ID) > 0 {
				_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{
					"code": -32601, "message": "method not found",
				}})
			}
		}
	}
}
