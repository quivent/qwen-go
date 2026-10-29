// Package agents runs bounded, independently persisted native coding agents.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"qwen-go/internal/core"
	"qwen-go/internal/toolkit"
)

// Options configures one application's child-agent pool. Client must support
// concurrent calls; provider.Client does. Approve must serialize UI prompts if
// multiple agents request permission simultaneously.
type Options struct {
	CWD, StoreDir, Model string
	ParentID             string
	Instructions         string
	Client               core.Streamer
	Approve              func(context.Context, string) bool
	AllowAll             bool
	MaxConcurrent        int
	Emit                 func(core.Event)
}

type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	opts     Options
	mu       sync.Mutex
	emitMu   sync.Mutex
	children map[string]*child
	slots    chan struct{}
	wg       sync.WaitGroup
	closed   bool
	initErr  error
}

type child struct {
	id, name, mode          string
	status, result, failure string
	created, updated        time.Time
	session                 *core.Session
	cancel                  context.CancelFunc
	done                    chan struct{}
}

type agentInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Status    string    `json:"status"`
	SessionID string    `json:"session_id"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	Updated   time.Time `json:"updated"`
}

func New(ctx context.Context, opts Options) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	lifetime, cancel := context.WithCancel(ctx)
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 3
	}
	if opts.MaxConcurrent > 16 {
		opts.MaxConcurrent = 16
	}
	m := &Manager{ctx: lifetime, cancel: cancel, opts: opts, children: map[string]*child{}, slots: make(chan struct{}, opts.MaxConcurrent)}
	if opts.CWD == "" {
		m.opts.CWD, m.initErr = os.Getwd()
	}
	if m.initErr == nil {
		m.opts.CWD, m.initErr = filepath.Abs(m.opts.CWD)
	}
	if m.initErr == nil {
		m.opts.CWD, m.initErr = filepath.EvalSymlinks(m.opts.CWD)
	}
	if m.opts.StoreDir == "" {
		var storeErr error
		m.opts.StoreDir, storeErr = core.SessionsDir()
		if m.initErr == nil {
			m.initErr = storeErr
		}
	}
	if m.opts.Client == nil && m.initErr == nil {
		m.initErr = errors.New("subagent model client is unavailable")
	}
	return m
}

func (m *Manager) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{
		def("spawn_agent", "Start a native concurrent subagent with a fresh conversation in this workspace. Default mode is read-only. Write mode requires approval, and each child mutation still requires approval. Children cannot spawn further agents.", `{"type":"object","properties":{"name":{"type":"string"},"prompt":{"type":"string"},"mode":{"type":"string","enum":["read-only","write"]}},"required":["name","prompt"],"additionalProperties":false}`),
		def("wait_agent", "Wait up to timeout_seconds (default 30, maximum 120) for one agent. Returns current status and its result when finished.", `{"type":"object","properties":{"id":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":120}},"required":["id"],"additionalProperties":false}`),
		def("list_agents", "List this application's child agents, including queued/running/completed status and their session IDs.", `{"type":"object","properties":{},"additionalProperties":false}`),
		def("send_agent_message", "Continue a finished agent in its saved conversation. Sending while it is running is rejected; wait for it first.", `{"type":"object","properties":{"id":{"type":"string"},"message":{"type":"string"}},"required":["id","message"],"additionalProperties":false}`),
		def("cancel_agent", "Cancel one queued or running agent and its active model/tool request.", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`),
	}
}
func def(name, description, parameters string) core.ToolDefinition {
	return core.ToolDefinition{Type: "function", Function: core.FunctionDefinition{Name: name, Description: description, Parameters: json.RawMessage(parameters)}}
}

type arguments struct {
	Name           string `json:"name"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode"`
	ID             string `json:"id"`
	Message        string `json:"message"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (m *Manager) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.initErr != nil {
		return "", m.initErr
	}
	var a arguments
	if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil {
		return "", fmt.Errorf("invalid subagent arguments: %w", err)
	}
	switch call.Function.Name {
	case "spawn_agent":
		return m.spawn(ctx, a)
	case "wait_agent":
		return m.wait(ctx, a)
	case "list_agents":
		m.mu.Lock()
		all := make([]agentInfo, 0, len(m.children))
		for _, c := range m.children {
			all = append(all, info(c))
		}
		m.mu.Unlock()
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		return encode(all)
	case "send_agent_message":
		return m.followup(ctx, a)
	case "cancel_agent":
		m.mu.Lock()
		c := m.children[a.ID]
		if c == nil {
			m.mu.Unlock()
			return "", fmt.Errorf("unknown agent %q", a.ID)
		}
		c.cancel()
		result := info(c)
		m.mu.Unlock()
		return encode(result)
	default:
		return "", fmt.Errorf("unknown subagent tool %q", call.Function.Name)
	}
}
func encode(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
func info(c *child) agentInfo {
	return agentInfo{ID: c.id, Name: c.name, Mode: c.mode, Status: c.status, SessionID: c.id, Result: c.result, Error: c.failure, Updated: c.updated}
}
func (m *Manager) availableLocked() error {
	if m.closed {
		return errors.New("subagent manager is closed")
	}
	return m.ctx.Err()
}

func (m *Manager) spawn(ctx context.Context, a arguments) (string, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Name == "" || len(a.Name) > 80 || a.Prompt == "" {
		return "", errors.New("name (1-80 bytes) and nonempty prompt are required")
	}
	if len(a.Prompt) > 256*1024 {
		return "", errors.New("subagent prompt exceeds 256 KiB")
	}
	mode := a.Mode
	if mode == "" {
		mode = "read-only"
	}
	if mode != "read-only" && mode != "write" {
		return "", errors.New("mode must be read-only or write")
	}
	if mode == "write" && !m.opts.AllowAll {
		if m.opts.Approve == nil || !m.opts.Approve(ctx, "Start write-enabled subagent "+a.Name+"? Each write and shell command will still request approval.\nTask: "+a.Prompt) {
			return "", errors.New("write-enabled subagent was not approved")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.availableLocked(); err != nil {
		return "", err
	}
	session := core.NewSession(m.opts.CWD, m.opts.Model)
	session.Title = "Agent " + a.Name
	session.ParentID = m.opts.ParentID
	if err := core.SaveSession(m.opts.StoreDir, session); err != nil {
		return "", fmt.Errorf("save child session: %w", err)
	}
	c := &child{id: session.ID, name: a.Name, mode: mode, created: time.Now().UTC(), session: session}
	m.children[c.id] = c
	m.startLocked(c, a.Prompt)
	return encode(info(c))
}

func (m *Manager) startLocked(c *child, prompt string) {
	ctx, cancel := context.WithCancel(m.ctx)
	c.cancel = cancel
	c.done = make(chan struct{})
	c.status = "queued"
	c.result = ""
	c.failure = ""
	c.updated = time.Now().UTC()
	done := c.done
	m.wg.Add(1)
	go m.run(ctx, c, prompt, done)
}

func (m *Manager) run(ctx context.Context, c *child, prompt string, done chan struct{}) {
	acquired := false
	result := ""
	failure := ""
	defer func() {
		if v := recover(); v != nil {
			failure = fmt.Sprintf("subagent failed: %v", v)
		}
		if acquired {
			<-m.slots
		}
		m.mu.Lock()
		switch {
		case ctx.Err() != nil:
			c.status = "cancelled"
		case failure != "":
			c.status = "failed"
		default:
			c.status = "completed"
		}
		c.result = result
		c.failure = failure
		c.updated = time.Now().UTC()
		status := c.status
		c.cancel()
		close(done)
		m.mu.Unlock()
		m.emit(core.Event{Kind: "agent", Name: c.id, Text: c.name + ": " + status})
		m.wg.Done()
	}()
	select {
	case m.slots <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	c.status = "running"
	c.updated = time.Now().UTC()
	m.mu.Unlock()
	m.emit(core.Event{Kind: "agent", Name: c.id, Text: c.name + ": running"})
	executor := &toolkit.Executor{CWD: m.opts.CWD, AllowAll: m.opts.AllowAll, Approve: func(ctx context.Context, description string) bool {
		return m.opts.Approve != nil && m.opts.Approve(ctx, "Agent "+c.name+":\n"+description)
	}}
	restricted := childExecutor{executor: executor, write: c.mode == "write"}
	definitions := toolkit.Definitions()
	if c.mode != "write" {
		definitions = definitions[:3]
	}
	runner := &core.Runner{Client: m.opts.Client, Executor: restricted, Tools: definitions, Session: c.session, StoreDir: m.opts.StoreDir, MaxTurns: 12}
	instruction := "You are a delegated subagent named " + c.name + ". Work only on this task and report concrete findings to the parent. You have an independent conversation. Other agents share this workspace; do not overwrite unrelated work. You cannot spawn further agents. "
	if c.mode != "write" {
		instruction += "You are read-only: do not edit files or execute shell commands. "
	}
	if m.opts.Instructions != "" {
		instruction += "\n\n" + m.opts.Instructions
	}
	messageStart := len(c.session.Messages)
	runner.Run(ctx, instruction+"\n\nTask:\n"+prompt, func(e core.Event) {
		switch e.Kind {
		case "error":
			failure = e.Text
		case "tool":
			// Tool names convey activity without copying potentially secret arguments.
			m.emit(core.Event{Kind: "agent", Name: c.id, Text: c.name + ": " + e.Name})
		}
	})
	// /new is not exposed to the child, but retain Runner's authoritative session.
	c.session = runner.Session
	for i := len(c.session.Messages) - 1; i >= messageStart; i-- {
		msg := c.session.Messages[i]
		if msg.Role == "assistant" && len(msg.ToolCalls) == 0 {
			result = msg.Content
			break
		}
	}
	if len(result) > 64*1024 {
		result = result[:64*1024] + "\n[Agent result truncated at 64 KiB; full result is in its saved session]"
	}
}

type childExecutor struct {
	executor *toolkit.Executor
	write    bool
}

func (e childExecutor) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	switch call.Function.Name {
	case "read_file", "list_directory", "search_files":
		return e.executor.Execute(ctx, call)
	case "write_file", "edit_file", "run_shell":
		if e.write {
			return e.executor.Execute(ctx, call)
		}
		return "", errors.New("read-only subagent cannot mutate files or execute shell commands")
	default:
		return "", fmt.Errorf("tool %q is not available to subagents", call.Function.Name)
	}
}

func (m *Manager) wait(ctx context.Context, a arguments) (string, error) {
	seconds := a.TimeoutSeconds
	if seconds == 0 {
		seconds = 30
	}
	if seconds < 1 || seconds > 120 {
		return "", errors.New("timeout_seconds must be 1-120")
	}
	m.mu.Lock()
	c := m.children[a.ID]
	if c == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("unknown agent %q", a.ID)
	}
	done := c.done
	m.mu.Unlock()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	m.mu.Lock()
	snapshot := info(c)
	m.mu.Unlock()
	return encode(snapshot)
}
func (m *Manager) followup(ctx context.Context, a arguments) (string, error) {
	if strings.TrimSpace(a.Message) == "" {
		return "", errors.New("nonempty message is required")
	}
	if len(a.Message) > 256*1024 {
		return "", errors.New("message exceeds 256 KiB")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.availableLocked(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := m.children[a.ID]
	if c == nil {
		return "", fmt.Errorf("unknown agent %q", a.ID)
	}
	select {
	case <-c.done:
	default:
		return "", errors.New("agent is still running; wait before sending a followup")
	}
	m.startLocked(c, a.Message)
	return encode(info(c))
}

// SetParentID associates future children with the newly selected parent session.
// Existing children retain the parent that originally created them.
func (m *Manager) SetParentID(id string) { m.mu.Lock(); m.opts.ParentID = id; m.mu.Unlock() }

// CancelAll requests cancellation without shutting down the reusable manager.
func (m *Manager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.children {
		c.cancel()
	}
}

// Close rejects further tasks, cancels all children and waits for their workers.
func (m *Manager) Close() { m.mu.Lock(); m.closed = true; m.cancel(); m.mu.Unlock(); m.wg.Wait() }
func (m *Manager) emit(e core.Event) {
	if m.opts.Emit == nil {
		return
	}
	m.emitMu.Lock()
	defer m.emitMu.Unlock()
	m.opts.Emit(e)
}
