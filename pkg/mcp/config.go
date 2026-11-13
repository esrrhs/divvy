package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ServerConfig describes one stdio MCP server. The shape intentionally matches
// the `mcpServers` block other coding agents use, so an existing configuration
// can be copied into `mcp.json` without rewriting.
type ServerConfig struct {
	Command  string            `json:"command"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type configFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
	Servers    map[string]ServerConfig `json:"servers"`
}

// ConfigPath returns the user-level MCP configuration path. Project config
// lives at <workspace>/.divvy/mcp.json and overrides a server of the same name.
func ConfigPath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return filepath.Join(dataDir, "mcp.json")
}

// LoadConfig merges user-level and project-level MCP server definitions.
// A missing file is not an error; a malformed file is, because silently
// ignoring a typo would look like "the server just has no tools".
func LoadConfig(dataDir, workspace string) (map[string]ServerConfig, error) {
	out := map[string]ServerConfig{}
	var firstErr error
	add := func(path string) {
		if path == "" {
			return
		}
		servers, err := readConfigFile(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		for name, cfg := range servers {
			out[name] = cfg
		}
	}
	add(ConfigPath(dataDir))
	if strings.TrimSpace(workspace) != "" {
		add(filepath.Join(workspace, ".divvy", "mcp.json"))
	}
	if firstErr != nil {
		return out, firstErr
	}
	for name, cfg := range out {
		if cfg.Disabled {
			delete(out, name)
			continue
		}
		if strings.TrimSpace(cfg.Command) == "" {
			return out, fmt.Errorf("mcp server %q has no command", name)
		}
	}
	return out, nil
}

func readConfigFile(path string) (map[string]ServerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read MCP config %s: %w", filepath.Base(path), err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var file configFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse MCP config %s: %w", filepath.Base(path), err)
	}
	merged := map[string]ServerConfig{}
	for name, cfg := range file.MCPServers {
		merged[name] = cfg
	}
	for name, cfg := range file.Servers {
		merged[name] = cfg
	}
	return merged, nil
}

// ServerNames returns configured names in sorted order, for stable logs and
// prompts regardless of map iteration.
func ServerNames(cfg map[string]ServerConfig) []string {
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
