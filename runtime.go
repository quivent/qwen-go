package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"qwen-go/internal/agents"
	"qwen-go/internal/core"
	"qwen-go/internal/extensions"
	"qwen-go/internal/mcp"
	"qwen-go/internal/toolkit"
)

// approvalBroker serializes prompts from the parent, children and MCP tools.
// A buffered reply channel makes a late cancellation/denial non-blocking.
type approvalBroker struct {
	gate chan struct{}
	emit func(core.Event)
}

func newApprovalBroker(emit func(core.Event)) *approvalBroker {
	return &approvalBroker{gate: make(chan struct{}, 1), emit: emit}
}
func (b *approvalBroker) approve(ctx context.Context, description string) bool {
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-b.gate }()
	if ctx.Err() != nil {
		return false
	}
	reply := make(chan bool, 1)
	b.emit(core.Event{Kind: "approval", Text: description, Reply: reply})
	select {
	case result := <-reply:
		return result && ctx.Err() == nil
	case <-ctx.Done():
		b.emit(core.Event{Kind: "approval_cancel", Reply: reply})
		return false
	}
}

type toolRuntime struct {
	base   *toolkit.Executor
	agents *agents.Manager
	mcp    *mcpHost
}

func (rt *toolRuntime) Definitions() []core.ToolDefinition {
	defs := append(toolkit.Definitions(), rt.agents.Definitions()...)
	return append(defs, rt.mcp.Definitions()...)
}
func (rt *toolRuntime) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	for _, d := range rt.agents.Definitions() {
		if d.Function.Name == call.Function.Name {
			return rt.agents.Execute(ctx, call)
		}
	}
	for _, d := range rt.mcp.Definitions() {
		if d.Function.Name == call.Function.Name {
			return rt.mcp.Execute(ctx, call)
		}
	}
	return rt.base.Execute(ctx, call)
}
func (rt *toolRuntime) Close() { rt.agents.Close(); rt.mcp.Close() }

type mcpHost struct {
	ctx     context.Context
	cwd     string
	servers map[string]extensions.MCPServer
	approve func(context.Context, string) bool
	yes     bool
	mu      sync.RWMutex
	clients map[string]*mcp.Client
	closed  bool
	cancels map[string]context.CancelFunc
}

func newMCPHost(ctx context.Context, cwd string, servers map[string]extensions.MCPServer, approve func(context.Context, string) bool, yes bool) *mcpHost {
	return &mcpHost{ctx: ctx, cwd: cwd, servers: servers, approve: approve, yes: yes, clients: map[string]*mcp.Client{}, cancels: map[string]context.CancelFunc{}}
}
func toolDef(name, description, schema string) core.ToolDefinition {
	return core.ToolDefinition{Type: "function", Function: core.FunctionDefinition{Name: name, Description: description, Parameters: json.RawMessage(schema)}}
}
func (h *mcpHost) Definitions() []core.ToolDefinition {
	if len(h.servers) == 0 {
		return nil
	}
	defs := []core.ToolDefinition{
		toolDef("list_mcp_servers", "List plugin MCP server names and whether connected. No processes are started.", `{"type":"object","properties":{},"additionalProperties":false}`),
		toolDef("connect_mcp_server", "Connect a configured local MCP tool server after explicit approval. Returns newly available namespaced tools. Use list_mcp_servers first.", `{"type":"object","properties":{"server":{"type":"string"}},"required":["server"],"additionalProperties":false}`),
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.clients))
	for name := range h.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		defs = append(defs, h.clients[name].Definitions()...)
	}
	return defs
}
func (h *mcpHost) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	h.mu.RLock()
	closed := h.closed
	h.mu.RUnlock()
	if closed {
		return "", fmt.Errorf("MCP host is closed")
	}
	switch call.Function.Name {
	case "list_mcp_servers":
		type record struct {
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
			Transport string `json:"transport"`
		}
		var records []record
		h.mu.RLock()
		for name, cfg := range h.servers {
			transport := "stdio"
			if cfg.URL != "" {
				transport = "remote (unsupported)"
			}
			records = append(records, record{name, h.clients[name] != nil, transport})
		}
		h.mu.RUnlock()
		sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
		b, _ := json.Marshal(records)
		return string(b), nil
	case "connect_mcp_server":
		var args struct {
			Server string `json:"server"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", err
		}
		cfg, ok := h.servers[args.Server]
		if !ok {
			return "", fmt.Errorf("unknown MCP server %q", args.Server)
		}
		if !cfg.Enabled {
			return "", fmt.Errorf("MCP server %q is disabled", args.Server)
		}
		if cfg.URL != "" || cfg.Transport != "" && cfg.Transport != "stdio" || cfg.Type != "" && cfg.Type != "stdio" {
			return "", fmt.Errorf("only local stdio MCP servers are supported")
		}
		h.mu.RLock()
		existing := h.clients[args.Server]
		h.mu.RUnlock()
		if existing != nil {
			return mcpToolNames(existing), nil
		}
		argsJSON, _ := json.Marshal(redactedArgs(cfg.Args))
		if !h.yes && (h.approve == nil || !h.approve(ctx, "Start MCP server "+args.Server+"\nExecutable: "+cfg.Command+"\nArguments: "+string(argsJSON)+"\nRuns with your user permissions; tool calls will also require approval.")) {
			return "", fmt.Errorf("MCP startup denied; no process started")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		lifetime, cancel := context.WithCancel(h.ctx)
		stop := context.AfterFunc(ctx, cancel)
		client, err := mcp.Connect(lifetime, args.Server, mcp.ServerConfig{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env}, h.cwd)
		stop()
		if err != nil {
			cancel()
			return "", err
		}
		if err := ctx.Err(); err != nil {
			client.Close()
			cancel()
			return "", err
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			client.Close()
			cancel()
			return "", fmt.Errorf("MCP host closed during startup")
		}
		if old := h.clients[args.Server]; old != nil {
			h.mu.Unlock()
			client.Close()
			cancel()
			return mcpToolNames(old), nil
		}
		h.clients[args.Server] = client
		h.cancels[args.Server] = cancel
		h.mu.Unlock()
		return mcpToolNames(client), nil
	}
	h.mu.RLock()
	var client *mcp.Client
	for _, candidate := range h.clients {
		for _, d := range candidate.Definitions() {
			if d.Function.Name == call.Function.Name {
				client = candidate
				break
			}
		}
		if client != nil {
			break
		}
	}
	h.mu.RUnlock()
	if client == nil {
		return "", fmt.Errorf("unknown MCP tool %q", call.Function.Name)
	}
	if !h.yes && (h.approve == nil || !h.approve(ctx, "Call external MCP tool "+call.Function.Name+"\nArguments: "+call.Function.Arguments)) {
		return "", fmt.Errorf("MCP tool call denied")
	}
	return client.Execute(ctx, call)
}
func mcpToolNames(c *mcp.Client) string {
	var names []string
	for _, d := range c.Definitions() {
		names = append(names, d.Function.Name)
	}
	b, _ := json.Marshal(names)
	return "Connected. Available tools: " + string(b)
}
func (h *mcpHost) Close() {
	h.mu.Lock()
	h.closed = true
	clients := h.clients
	cancels := h.cancels
	h.clients = map[string]*mcp.Client{}
	h.cancels = map[string]context.CancelFunc{}
	h.mu.Unlock()
	for name, client := range clients {
		if cancel := cancels[name]; cancel != nil {
			cancel()
		}
		_ = client.Close()
	}
}

func redactedArgs(args []string) []string {
	out := append([]string(nil), args...)
	hideNext := false
	for i, arg := range out {
		if hideNext {
			out[i] = "[redacted]"
			hideNext = false
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key := strings.ToLower(strings.SplitN(arg, "=", 2)[0])
		if strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "secret") || strings.Contains(key, "api-key") || strings.Contains(key, "api_key") {
			if parts := strings.SplitN(arg, "=", 2); len(parts) == 2 {
				out[i] = parts[0] + "=[redacted]"
			} else {
				hideNext = true
			}
		}
	}
	return out
}

func extensionCommand(reg *extensions.Registry, manager *agents.Manager, input string, emit func(core.Event)) bool {
	switch strings.TrimSpace(input) {
	case "/plugins":
		emit(core.Event{Kind: "info", Text: reg.List()})
		return true
	case "/skills":
		var rows []string
		for _, s := range reg.Skills {
			rows = append(rows, s.Plugin+":"+s.Name+" — "+s.Description)
		}
		if len(rows) == 0 {
			rows = append(rows, "No plugin skills installed.")
		}
		emit(core.Event{Kind: "info", Text: strings.Join(rows, "\n")})
		return true
	case "/agents":
		result, err := manager.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: "list_agents", Arguments: "{}"}})
		if err != nil {
			result = err.Error()
		}
		emit(core.Event{Kind: "info", Text: result})
		return true
	}
	return false
}
