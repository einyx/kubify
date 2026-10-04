package portal

import (
	"context"
	"encoding/json"
	"fmt"
)

// MCPCaller invokes MCP tools in-process. main.go wires an adapter around
// the mcpserver (portal cannot import mcpserver without an import cycle).
type MCPCaller interface {
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
	ListTools(ctx context.Context) ([]string, error)
	ToolSchemas(ctx context.Context) (json.RawMessage, error)
	ToolCount() int
	AuthEnabled() bool
}

// SetMCPCaller wires the MCP bridge. Passing nil disables MCP endpoints.
func (p *Portal) SetMCPCaller(c MCPCaller) { p.mcpCaller = c }

// mcpToolDenylist are tools the portal never exposes — the portal has its
// own guarded equivalents, and agent-level destructive operations belong to
// agent sessions, not a shared web UI.
var mcpToolDenylist = map[string]bool{
	"delete_stack": true,
}

func (p *Portal) mcpAllowed(name string) bool { return !mcpToolDenylist[name] }

// MCPInfo is the status payload for the portal's MCP section.
type MCPInfo struct {
	Enabled  bool     `json:"enabled"`
	AuthMode string   `json:"authMode"`
	Tools    []string `json:"tools"`
}

// MCPInfo reports the MCP bridge state and available tool names.
func (p *Portal) MCPInfo(ctx context.Context) MCPInfo {
	if p.mcpCaller == nil {
		return MCPInfo{Enabled: false, AuthMode: "disabled"}
	}
	tools, _ := p.mcpCaller.ListTools(ctx)
	mode := "none (local stdio or unauthenticated SSE)"
	if p.mcpCaller.AuthEnabled() {
		mode = "bearer token"
	}
	return MCPInfo{Enabled: true, AuthMode: mode, Tools: tools}
}

// MCPCall invokes an allowed tool through the bridge.
func (p *Portal) MCPCall(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if p.mcpCaller == nil {
		return nil, fmt.Errorf("MCP is not wired into this portal")
	}
	if !p.mcpAllowed(name) {
		return nil, fmt.Errorf("tool %q is blocked in the portal (destructive)", name)
	}
	return p.mcpCaller.CallTool(ctx, name, arguments)
}
