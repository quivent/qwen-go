package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"qwen-go/internal/core"
)

func helperConfig(t *testing.T, mode string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Command: exe, Args: []string{"-test.run=^TestMCPHelperProcess$"}, Env: map[string]string{"QWEN_GO_MCP_HELPER": "1", "QWEN_GO_MCP_MODE": mode}}
}
func connectHelper(t *testing.T, mode string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := Connect(ctx, "test/server", helperConfig(t, mode), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("QWEN_GO_MCP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("QWEN_GO_MCP_MODE")
	encoder := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	send := func(value any) { mu.Lock(); defer mu.Unlock(); _ = encoder.Encode(value) }
	initialized := false
	var pendingToolsID json.RawMessage
	pingAnswered, unsupportedAnswered := false, false
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		respond := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			if mode == "timeout" {
				continue
			}
			if mode == "invalid" {
				fmt.Fprintln(os.Stdout, "not json")
				continue
			}
			if mode == "oversized" {
				fmt.Fprintln(os.Stdout, strings.Repeat("x", maxMessageBytes+1))
				continue
			}
			version := protocolVersion
			if mode == "version" {
				version = "1900-01-01"
			}
			respond(map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "native-go-helper", "version": "1"}})
		case "notifications/initialized":
			initialized = true
		case "tools/list":
			if !initialized {
				os.Exit(3)
			}
			if mode == "listfail" {
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32603, "message": "listing failed"}})
				continue
			}
			if mode == "badlist" {
				respond(map[string]any{"tools": "not a list"})
				continue
			}
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &params)
			tools := []map[string]any{{"name": "read/file", "description": "Read a file", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}, {"name": "read_file", "description": "A distinct tool", "inputSchema": map[string]any{"type": "object"}}}
			if mode == "pagination" && params.Cursor == "" {
				respond(map[string]any{"tools": tools[:1], "nextCursor": "page2"})
				continue
			}
			if mode == "pagination" {
				tools = tools[1:]
			}
			if mode == "serverrequest" {
				send(map[string]any{"jsonrpc": "2.0", "id": "server-ping", "method": "ping"})
				send(map[string]any{"jsonrpc": "2.0", "id": "server-unsupported", "method": "sampling/createMessage", "params": map[string]any{}})
				pendingToolsID = append(json.RawMessage(nil), req.ID...)
				continue
			}
			respond(map[string]any{"tools": tools})
		case "tools/call":
			var params struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if _, wait := params.Arguments["wait"]; wait {
				continue
			}
			isError := params.Arguments["fail"] != nil
			text := params.Name
			if value := params.Arguments["text"]; value != nil {
				_ = json.Unmarshal(value, &text)
			}
			if mode == "large" {
				text = strings.Repeat("z", 600*1024)
			}
			respond(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError})
		case "notifications/cancelled":
		case "": // Server waits for both client replies before finishing discovery.
			if string(req.ID) == `"server-ping"` && req.Error == nil && len(req.Result) > 0 {
				pingAnswered = true
			}
			if string(req.ID) == `"server-unsupported"` && req.Error != nil && req.Error.Code == -32601 {
				unsupportedAnswered = true
			}
			if pingAnswered && unsupportedAnswered && len(pendingToolsID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": pendingToolsID, "result": map[string]any{"tools": []map[string]any{{"name": "read/file", "inputSchema": map[string]any{"type": "object"}}, {"name": "read_file", "inputSchema": map[string]any{"type": "object"}}}}})
				pendingToolsID = nil
			}
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
	os.Exit(0)
}

func TestNativeMCPHandshakeDiscoveryAndExecution(t *testing.T) {
	c := connectHelper(t, "normal")
	defs := c.Definitions()
	if len(defs) != 2 {
		t.Fatalf("got %d tools", len(defs))
	}
	if defs[0].Function.Name == defs[1].Function.Name {
		t.Fatal("sanitized tool names collided")
	}
	for _, def := range defs {
		if len(def.Function.Name) > 64 || !strings.HasPrefix(def.Function.Name, "mcp_test_server_") {
			t.Fatalf("bad exported tool name %q", def.Function.Name)
		}
	}
	out, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: defs[0].Function.Name, Arguments: `{"text":"native plugin works"}`}})
	if err != nil || out != "native plugin works" {
		t.Fatalf("got %q, %v", out, err)
	}
	// Definitions returns deep copies so callers cannot alter the registered schema.
	defs[0].Function.Parameters[0] = 'x'
	if c.Definitions()[0].Function.Parameters[0] == 'x' {
		t.Fatal("schema aliases client state")
	}
}
func TestConcurrentMCPCalls(t *testing.T) {
	c := connectHelper(t, "normal")
	name := c.Definitions()[0].Function.Name
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("result-%d", i)
			args, _ := json.Marshal(map[string]string{"text": want})
			out, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: string(args)}})
			if err != nil || out != want {
				t.Errorf("got %q, %v, want %q", out, err, want)
			}
		}(i)
	}
	wg.Wait()
}
func TestMCPTimeoutDoesNotBreakLaterCalls(t *testing.T) {
	c := connectHelper(t, "normal")
	name := c.Definitions()[0].Function.Name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Execute(ctx, core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: `{"wait":true}`}})
	if err == nil {
		t.Fatal("expected timeout")
	}
	out, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: `{}`}})
	if err != nil || out != "read/file" {
		t.Fatalf("connection did not recover: %q %v", out, err)
	}
}
func TestMCPConnectRejectsMalformedAndFailedResponses(t *testing.T) {
	for _, mode := range []string{"invalid", "listfail", "badlist", "version", "timeout", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			c, err := Connect(ctx, "broken", helperConfig(t, mode), t.TempDir())
			if err == nil {
				c.Close()
				t.Fatal("expected startup failure")
			}
		})
	}
}
func TestMCPPaginationAndServerRequests(t *testing.T) {
	for _, mode := range []string{"pagination", "serverrequest"} {
		t.Run(mode, func(t *testing.T) {
			c := connectHelper(t, mode)
			if len(c.Definitions()) != 2 {
				t.Fatal("missing tools")
			}
		})
	}
}
func TestMCPToolErrorsArgumentsAndClosedClient(t *testing.T) {
	c := connectHelper(t, "normal")
	name := c.Definitions()[0].Function.Name
	for _, args := range []string{"not-json", "null", "[]"} {
		if _, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: args}}); err == nil {
			t.Fatalf("accepted bad args %q", args)
		}
	}
	out, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: `{"fail":true,"text":"denied"}`}})
	if err == nil || out != "denied" {
		t.Fatalf("tool error lost: %q %v", out, err)
	}
	c.Close()
	c.Close()
	if _, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: `{}`}}); err == nil {
		t.Fatal("closed client allowed execution")
	}
}
func TestMCPBoundsLargeOutput(t *testing.T) {
	c := connectHelper(t, "large")
	out, err := c.Execute(context.Background(), core.ToolCall{Function: core.FunctionCall{Name: c.Definitions()[0].Function.Name, Arguments: `{}`}})
	if err != nil || len(out) > 513*1024 || !strings.HasSuffix(out, "[output truncated]") {
		t.Fatalf("unbounded output length=%d err=%v", len(out), err)
	}
}
func TestMCPNamespacesRemainDistinct(t *testing.T) {
	if functionName("a-b", "x") == functionName("a_b", "x") {
		t.Fatal("server names collide")
	}
	if functionName("a", "read/file") == functionName("a", "read_file") {
		t.Fatal("tool names collide")
	}
}
