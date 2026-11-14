package agent

import (
	"context"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/mcp"
)

// setupMCP starts the stdio MCP servers configured for this user/project. It
// fails open: a missing/broken config must not block a turn, because MCP is an
// optional extension, not a requirement. Servers run in the active workspace
// (the isolated mirror when approval is manual), so their file operations see
// the same tree as the built-in tools.
func (d *DirectAgent) setupMCP(ctx context.Context) {
	workdir := d.toolSandbox().Root
	cfg, err := mcp.LoadConfig(d.o.cfg.DataDir, workdir)
	if err != nil {
		d.o.log.Warnf("MCP config: %v", err)
	}
	if len(cfg) == 0 {
		return
	}
	rt, err := mcp.Start(ctx, cfg, workdir, func(format string, args ...any) {
		d.o.log.Warnf(format, args...)
	})
	if err != nil {
		d.o.log.Warnf("MCP partially unavailable: %v", err)
	}
	if rt == nil {
		return
	}
	d.mcp = rt
	d.o.log.Infof("MCP: %d server(s), %d tool(s) available", rt.ServerCount(), len(rt.ToolDefs()))
}

func (d *DirectAgent) closeMCP() {
	if d.mcp != nil {
		d.mcp.Close()
		d.mcp = nil
	}
}

func (d *DirectAgent) mcpGuide() string {
	if d.mcp == nil {
		return ""
	}
	guide := d.mcp.Guide()
	if guide == "" {
		return ""
	}
	return "\n" + guide
}

func (d *DirectAgent) mcpToolDefs() []llm.Tool {
	if d.mcp == nil {
		return nil
	}
	return d.mcp.ToolDefs()
}
