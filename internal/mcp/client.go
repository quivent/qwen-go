// Package mcp connects native clients to MCP servers over newline-delimited JSON-RPC.
// It starts the configured executable directly; no JavaScript host is involved.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qwen-go/internal/core"
)

const protocolVersion = "2024-11-05"
const maxMessageBytes = 8 * 1024 * 1024

var ErrClosed = errors.New("MCP connection closed")

type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type reply struct {
	result json.RawMessage
	err    error
}

// Client is a running MCP stdio connection. Definitions are immutable after Connect.
type Client struct {
	name        string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	ctx         context.Context
	cancel      context.CancelFunc
	next        atomic.Int64
	writeMu     sync.Mutex
	mu          sync.Mutex
	pending     map[int64]chan reply
	terminalErr error
	done        chan struct{}
	waited      chan struct{}
	closeOnce   sync.Once
	definitions []core.ToolDefinition
	names       map[string]string
	stderr      *cappedLog
}

// Connect starts a server and performs initialize, initialized, and tools/list.
// The server's lifetime is bounded by ctx. Start it only after user approval.
func Connect(ctx context.Context, name string, cfg ServerConfig, cwd string) (*Client, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("MCP server name is empty")
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("MCP executable is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(lifetime, cfg.Command, cfg.Args...)
	cmd.Dir = cwd
	cmd.Env = mergedEnv(cfg.Env)
	configureProcess(cmd)
	cmd.Cancel = func() error { return terminateProcess(cmd, true) }
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	c := &Client{name: name, cmd: cmd, stdin: stdin, ctx: lifetime, cancel: cancel, pending: make(map[int64]chan reply), done: make(chan struct{}), waited: make(chan struct{}), names: make(map[string]string), stderr: &cappedLog{max: 16 * 1024}}
	cmd.Stderr = c.stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		return nil, fmt.Errorf("start MCP %s: %w", name, err)
	}
	go c.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		close(c.waited)
		if err == nil {
			err = io.EOF
		}
		c.fail(fmt.Errorf("MCP %s exited: %w", name, err))
	}()
	startup, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	fail := func(err error) (*Client, error) { c.Close(); return nil, fmt.Errorf("connect MCP %s: %w", name, err) }
	result, err := c.request(startup, "initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "qwen-go", "version": "0.1.0"}})
	if err != nil {
		return fail(err)
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	if err = json.Unmarshal(result, &init); err != nil {
		return fail(fmt.Errorf("invalid initialize response: %w", err))
	}
	if init.ProtocolVersion != protocolVersion {
		return fail(fmt.Errorf("unsupported MCP protocol version %q", init.ProtocolVersion))
	}
	if err = c.notify(startup, "notifications/initialized", nil); err != nil {
		return fail(err)
	}
	// Servers without tools are valid MCP servers and expose no model functions.
	if _, hasTools := init.Capabilities["tools"]; !hasTools {
		return c, nil
	}
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err = c.request(startup, "tools/list", params)
		if err != nil {
			return fail(err)
		}
		var listing struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err = json.Unmarshal(result, &listing); err != nil {
			return fail(fmt.Errorf("invalid tools/list response: %w", err))
		}
		var listingFields map[string]json.RawMessage
		if json.Unmarshal(result, &listingFields) != nil || len(listingFields["tools"]) == 0 || bytes.TrimSpace(listingFields["tools"])[0] != '[' {
			return fail(errors.New("tools/list response must contain a tools array"))
		}
		for _, tool := range listing.Tools {
			if strings.TrimSpace(tool.Name) == "" {
				return fail(errors.New("MCP tool has empty name"))
			}
			if len(c.definitions) >= 4096 {
				return fail(errors.New("MCP server exposes too many tools"))
			}
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			var shape map[string]json.RawMessage
			if err = json.Unmarshal(schema, &shape); err != nil || shape == nil {
				return fail(fmt.Errorf("invalid input schema for %s", tool.Name))
			}
			exported := functionName(name, tool.Name)
			if _, exists := c.names[exported]; exists {
				return fail(fmt.Errorf("duplicate or colliding MCP tool name %q", tool.Name))
			}
			c.names[exported] = tool.Name
			c.definitions = append(c.definitions, core.ToolDefinition{Type: "function", Function: core.FunctionDefinition{Name: exported, Description: "[MCP " + name + "] " + tool.Description, Parameters: append(json.RawMessage(nil), schema...)}})
		}
		if listing.NextCursor == "" {
			return c, nil
		}
		cursor = listing.NextCursor
		if seenCursors[cursor] {
			return fail(errors.New("MCP tools/list repeated pagination cursor"))
		}
		seenCursors[cursor] = true
	}
	return fail(errors.New("MCP tools/list pagination limit reached"))
}

func mergedEnv(override map[string]string) []string {
	values := map[string]string{}
	for _, line := range os.Environ() {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	for k, v := range override {
		values[k] = v
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}
func safeName(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= limit {
			break
		}
	}
	if b.Len() == 0 {
		return "server"
	}
	return b.String()
}
func functionName(server, tool string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	return "mcp_" + safeName(server, 16) + "_" + safeName(tool, 26) + "_" + hex.EncodeToString(sum[:5])
}

func (c *Client) Definitions() []core.ToolDefinition {
	defs := make([]core.ToolDefinition, len(c.definitions))
	copy(defs, c.definitions)
	for i := range defs {
		defs[i].Function.Parameters = append(json.RawMessage(nil), defs[i].Function.Parameters...)
	}
	return defs
}

// Execute invokes a discovered tool. The caller must obtain user approval first.
func (c *Client) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	original, ok := c.names[call.Function.Name]
	if !ok {
		return "", fmt.Errorf("unknown MCP tool %q", call.Function.Name)
	}
	raw := call.Function.Arguments
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return "", errors.New("MCP tool arguments must be a JSON object")
	}
	if len(raw) > maxMessageBytes/2 {
		return "", errors.New("MCP tool arguments exceed size limit")
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := c.request(bounded, "tools/call", map[string]any{"name": original, "arguments": args})
	if err != nil {
		return "", err
	}
	var value struct {
		Content           []json.RawMessage `json:"content"`
		IsError           bool              `json:"isError"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
	}
	if err = json.Unmarshal(result, &value); err != nil {
		return "", fmt.Errorf("invalid MCP tool result: %w", err)
	}
	var resultFields map[string]json.RawMessage
	if json.Unmarshal(result, &resultFields) != nil || resultFields == nil {
		return "", errors.New("MCP tool result must be an object")
	}
	if content, ok := resultFields["content"]; ok {
		if len(bytes.TrimSpace(content)) == 0 || bytes.TrimSpace(content)[0] != '[' {
			return "", errors.New("MCP tool result content must be an array")
		}
	} else if len(value.StructuredContent) == 0 {
		return "", errors.New("MCP tool result is missing content")
	}
	var text []string
	for _, block := range value.Content {
		var item struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		}
		if json.Unmarshal(block, &item) != nil {
			return "", errors.New("invalid MCP content block")
		}
		switch item.Type {
		case "text":
			text = append(text, item.Text)
		case "resource":
			if item.Resource.Text != "" {
				text = append(text, item.Resource.Text)
			} else {
				text = append(text, "[resource: "+item.Resource.URI+"]")
			}
		case "image", "audio":
			text = append(text, "["+item.Type+" "+item.MimeType+" returned; binary content omitted]")
		default:
			text = append(text, "[unsupported MCP content type: "+item.Type+"]")
		}
	}
	if len(text) == 0 && len(value.StructuredContent) > 0 {
		text = append(text, string(value.StructuredContent))
	}
	output := strings.Join(text, "\n")
	if len(output) > 512*1024 {
		output = output[:512*1024] + "\n[output truncated]"
	}
	if value.IsError {
		return output, fmt.Errorf("MCP tool %s failed: %s", original, truncate(output, 2000))
	}
	return output, nil
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.next.Add(1)
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.terminalErr != nil {
		err := c.terminalErr
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case response := <-ch:
		return response.result, response.err
	case <-ctx.Done():
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, 250*time.Millisecond)
			defer cancel()
			_ = c.notify(ctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": "client request canceled"})
		}()
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.terminalErr
		c.mu.Unlock()
		return nil, err
	}
}
func (c *Client) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(ctx, msg)
}
func (c *Client) write(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxMessageBytes {
		return errors.New("MCP outgoing message exceeds size limit")
	}
	data = append(data, '\n')
	written := make(chan error, 1)
	go func() { c.writeMu.Lock(); defer c.writeMu.Unlock(); _, err := c.stdin.Write(data); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			c.fail(err)
		}
		return err
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}
func (c *Client) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	for scanner.Scan() {
		raw := scanner.Bytes()
		var msg envelope
		if err := json.Unmarshal(raw, &msg); err != nil || msg.JSONRPC != "2.0" {
			c.fail(errors.New("MCP server wrote an invalid JSON-RPC message"))
			c.cancel()
			return
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 {
				// Sampling, elicitation and roots are not advertised. Answer rather than
				// deadlock a server waiting for a client capability we do not implement.
				response := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
				if msg.Method == "ping" {
					response["result"] = map[string]any{}
				} else {
					response["error"] = rpcError{Code: -32601, Message: "client method not supported"}
				}
				go func() {
					ctx, cancel := context.WithTimeout(c.ctx, time.Second)
					defer cancel()
					_ = c.write(ctx, response)
				}()
			}
			continue
		}
		if len(msg.ID) == 0 {
			c.fail(errors.New("MCP response missing id"))
			c.cancel()
			return
		}
		id, err := strconv.ParseInt(string(msg.ID), 10, 64)
		if err != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		result := reply{result: msg.Result}
		if msg.Error != nil {
			result.err = fmt.Errorf("MCP %s error %d: %s", c.name, msg.Error.Code, msg.Error.Message)
		} else if len(msg.Result) == 0 {
			result.err = errors.New("MCP response missing result")
		}
		select {
		case ch <- result:
		default:
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(fmt.Errorf("MCP %s transport: %w", c.name, err))
	c.cancel()
}
func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalErr != nil {
		return
	}
	c.terminalErr = err
	close(c.done)
	for _, ch := range c.pending {
		select {
		case ch <- reply{err: err}:
		default:
		}
	}
}

// Close releases the entire subprocess group and unblocks outstanding requests.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		_ = c.stdin.Close()
		select {
		case <-c.waited:
		default:
			_ = terminateProcess(c.cmd, false)
		}
		select {
		case <-c.waited:
		case <-time.After(300 * time.Millisecond):
			c.cancel()
			_ = terminateProcess(c.cmd, true)
			select {
			case <-c.waited:
			case <-time.After(time.Second):
			}
		}
		c.cancel()
	})
	return nil
}

type cappedLog struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func (w *cappedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.max - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, p[:min(remaining, n)]...)
	}
	return n, nil
}
