package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qwen-go/internal/agents"
	"qwen-go/internal/builtinmcp"
	"qwen-go/internal/core"
	"qwen-go/internal/extensions"
	"qwen-go/internal/toolkit"
)

// A native Go test subprocess acts as an MCP server. The input trace proves
// denied calls never reached the external process, beyond checking error text.
func TestRuntimeMCPHelper(t *testing.T) {
	if os.Getenv("QWEN_GO_RUNTIME_HELPER") != "1" {
		return
	}
	marker := os.Getenv("QWEN_GO_RUNTIME_TRACE")
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = file.WriteString("started\n")
	defer file.Close()
	if os.Getenv("QWEN_GO_RUNTIME_HANG") == "1" {
		time.Sleep(time.Hour)
		return
	}
	cwd, _ := os.Getwd()
	if err = builtinmcp.Serve(context.Background(), io.TeeReader(os.Stdin, file), os.Stdout, cwd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}
func runtimeMCPServer(t *testing.T) (extensions.MCPServer, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "trace")
	return extensions.MCPServer{Enabled: true, Command: exe, Args: []string{"-test.run=^TestRuntimeMCPHelper$"}, Env: map[string]string{"QWEN_GO_RUNTIME_HELPER": "1", "QWEN_GO_RUNTIME_TRACE": trace}}, trace
}
func runtimeCall(name string, a map[string]any) core.ToolCall {
	b, _ := json.Marshal(a)
	return core.ToolCall{ID: name, Type: "function", Function: core.FunctionCall{Name: name, Arguments: string(b)}}
}

func TestMCPStartupDenialStartsNoProcess(t *testing.T) {
	cfg, trace := runtimeMCPServer(t)
	h := newMCPHost(context.Background(), t.TempDir(), map[string]extensions.MCPServer{"native": cfg}, func(context.Context, string) bool { return false }, false)
	defer h.Close()
	if _, err := h.Execute(context.Background(), runtimeCall("connect_mcp_server", map[string]any{"server": "native"})); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("wanted denial, got %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("denied startup created process trace: %v", err)
	}
	if len(h.Definitions()) != 2 {
		t.Fatal("denied server exposed tools")
	}
}

func TestMCPCallDenialNeverReachesServer(t *testing.T) {
	cfg, trace := runtimeMCPServer(t)
	var approvals atomic.Int32
	h := newMCPHost(context.Background(), t.TempDir(), map[string]extensions.MCPServer{"native": cfg}, func(context.Context, string) bool { return approvals.Add(1) == 1 }, false)
	defer h.Close()
	initial := len(h.Definitions())
	if _, err := h.Execute(context.Background(), runtimeCall("connect_mcp_server", map[string]any{"server": "native"})); err != nil {
		t.Fatal(err)
	}
	defs := h.Definitions()
	if len(defs) != initial+2 {
		t.Fatalf("dynamic tools not exposed: %d -> %d", initial, len(defs))
	}
	var name string
	for _, d := range defs {
		if strings.Contains(d.Function.Name, "git_status") {
			name = d.Function.Name
			break
		}
	}
	if name == "" {
		t.Fatal("missing native git tool")
	}
	if _, err := h.Execute(context.Background(), runtimeCall(name, map[string]any{})); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("wanted call denial, got %v", err)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "started\n") || strings.Contains(string(data), `"method":"tools/call"`) {
		t.Fatalf("denied call reached server: %s", data)
	}
	if approvals.Load() != 2 {
		t.Fatalf("startup and call approvals = %d", approvals.Load())
	}
}

func TestApprovalBrokerSerializesConcurrentRequests(t *testing.T) {
	events := make(chan core.Event, 4)
	broker := newApprovalBroker(func(e core.Event) { events <- e })
	results := make(chan bool, 2)
	go func() { results <- broker.approve(context.Background(), "first") }()
	first := <-events
	go func() { results <- broker.approve(context.Background(), "second") }()
	select {
	case <-events:
		t.Fatal("concurrent prompts shown at once")
	case <-time.After(30 * time.Millisecond):
	}
	first.Reply <- true
	second := <-events
	second.Reply <- false
	a, b := <-results, <-results
	if a == b {
		t.Fatalf("wanted one allow and one denial, got %v %v", a, b)
	}
}

func TestApprovalBrokerCancellationReleasesGate(t *testing.T) {
	events := make(chan core.Event, 4)
	broker := newApprovalBroker(func(e core.Event) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() { result <- broker.approve(ctx, "cancel me") }()
	first := <-events
	cancel()
	if <-result {
		t.Fatal("cancelled approval allowed action")
	}
	cancelled := <-events
	if cancelled.Kind != "approval_cancel" || cancelled.Reply != first.Reply {
		t.Fatalf("wrong cancellation notification: %+v", cancelled)
	}
	// A stale response must never block the terminal or approve another request.
	first.Reply <- true
	go func() { result <- broker.approve(context.Background(), "next") }()
	second := <-events
	if second.Kind != "approval" || second.Reply == first.Reply {
		t.Fatalf("next approval reused stale dialog: %+v", second)
	}
	second.Reply <- false
	if <-result {
		t.Fatal("late response approved different request")
	}
	held := make(chan bool, 1)
	go func() { held <- broker.approve(context.Background(), "hold gate") }()
	third := <-events
	waiting, cancelWaiting := context.WithCancel(context.Background())
	queued := make(chan bool, 1)
	go func() { queued <- broker.approve(waiting, "queued") }()
	cancelWaiting()
	if <-queued {
		t.Fatal("cancelled waiter acquired permission")
	}
	third.Reply <- false
	<-held
}

type runtimeStreamFunc func(context.Context, []core.Message, []core.ToolDefinition, func(string)) (core.Message, error)

func (f runtimeStreamFunc) Stream(ctx context.Context, m []core.Message, d []core.ToolDefinition, emit func(string)) (core.Message, error) {
	return f(ctx, m, d, emit)
}

func TestRunnerDynamicMCPAndNativeSubagentRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	store := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	cfg, _ := runtimeMCPServer(t)
	var childCalls atomic.Int32
	var lifecycleMu sync.Mutex
	var lifecycle []core.Event
	var parentSteps atomic.Int32
	model := runtimeStreamFunc(func(_ context.Context, messages []core.Message, defs []core.ToolDefinition, delta func(string)) (core.Message, error) {
		for _, msg := range messages {
			if msg.Role == "user" && strings.Contains(msg.Content, "You are a delegated subagent named") {
				childCalls.Add(1)
				if len(defs) != 3 {
					t.Errorf("child escaped readonly tool set: %d", len(defs))
				}
				return core.Message{Content: "native child finding"}, nil
			}
		}
		step := parentSteps.Add(1)
		makeAnswer := func(id, name string, args map[string]any) core.Message {
			call := runtimeCall(name, args)
			call.ID = id
			return core.Message{ToolCalls: []core.ToolCall{call}}
		}
		switch step {
		case 1:
			for _, d := range defs {
				if strings.Contains(d.Function.Name, "git_status") {
					t.Error("MCP tool exposed before connection")
				}
			}
			return makeAnswer("connect", "connect_mcp_server", map[string]any{"server": "native"}), nil
		case 2:
			found := false
			for _, d := range defs {
				if strings.Contains(d.Function.Name, "git_status") {
					found = true
				}
			}
			if !found {
				t.Error("Runner did not refresh tool definitions after connect")
			}
			return makeAnswer("spawn", "spawn_agent", map[string]any{"name": "reader", "prompt": "Inspect independently"}), nil
		case 3:
			last := messages[len(messages)-1]
			var info struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(last.Content), &info); err != nil {
				return core.Message{}, err
			}
			if info.ID == "" {
				return core.Message{}, fmt.Errorf("no child ID: %s", last.Content)
			}
			return makeAnswer("wait", "wait_agent", map[string]any{"id": info.ID, "timeout_seconds": 2}), nil
		case 4:
			last := messages[len(messages)-1]
			if !strings.Contains(last.Content, "native child finding") || !strings.Contains(last.Content, "completed") {
				t.Errorf("child result lost: %s", last.Content)
			}
			for _, d := range defs {
				if strings.Contains(d.Function.Name, "git_status") {
					return makeAnswer("git", d.Function.Name, map[string]any{}), nil
				}
			}
			return core.Message{}, fmt.Errorf("missing Git tool")
		case 5:
			last := messages[len(messages)-1]
			if !strings.Contains(last.Content, "branch") && !strings.Contains(last.Content, "No commits") {
				t.Errorf("Git status result missing: %s", last.Content)
			}
			delta("Completed with native tools and child.")
			return core.Message{Content: "Completed with native tools and child."}, nil
		default:
			return core.Message{}, fmt.Errorf("unexpected parent round %d", step)
		}
	})
	session := core.NewSession(workspace, "test")
	manager := agents.New(context.Background(), agents.Options{CWD: workspace, StoreDir: store, Model: "test", ParentID: session.ID, Client: model, Emit: func(e core.Event) { lifecycleMu.Lock(); lifecycle = append(lifecycle, e); lifecycleMu.Unlock() }})
	host := newMCPHost(context.Background(), workspace, map[string]extensions.MCPServer{"native": cfg}, nil, true)
	runtime := &toolRuntime{base: &toolkit.Executor{CWD: workspace}, agents: manager, mcp: host}
	defer runtime.Close()
	runner := &core.Runner{Client: model, Executor: runtime, ToolsFunc: runtime.Definitions, Session: session, StoreDir: store}
	var output strings.Builder
	runner.Run(context.Background(), "Use an independent child and native Git plugin", func(e core.Event) {
		if e.Kind == "error" {
			t.Error(e.Text)
		}
		if e.Kind == "delta" {
			output.WriteString(e.Text)
		}
	})
	if parentSteps.Load() != 5 || childCalls.Load() != 1 || output.String() != "Completed with native tools and child." {
		t.Fatalf("round trip parent=%d children=%d output=%q", parentSteps.Load(), childCalls.Load(), output.String())
	}
	loaded, err := core.LoadSession(store, session.ID)
	if err != nil || len(loaded.Messages) != 10 {
		t.Fatalf("saved parent history %+v %v", loaded, err)
	}
	manager.Close()
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if len(lifecycle) < 2 {
		t.Fatalf("missing visible lifecycle events: %+v", lifecycle)
	}
	for _, e := range lifecycle {
		if e.Kind != "agent" {
			t.Fatalf("child event polluted parent stream: %+v", e)
		}
	}
}

func TestMCPStartupHonorsRequestCancellation(t *testing.T) {
	cfg, trace := runtimeMCPServer(t)
	cfg.Env["QWEN_GO_RUNTIME_HANG"] = "1"
	app, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	h := newMCPHost(app, t.TempDir(), map[string]extensions.MCPServer{"hang": cfg}, nil, true)
	defer h.Close()
	request, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	done := make(chan error, 1)
	go func() {
		_, err := h.Execute(request, runtimeCall("connect_mcp_server", map[string]any{"server": "hang"}))
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(trace); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancelApp()
			<-done
			t.Fatal("helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelRequest()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled startup succeeded")
		}
	case <-time.After(time.Second):
		cancelApp()
		<-done
		t.Fatal("MCP startup ignored request cancellation")
	}
}

func TestMCPHostCannotRestartAfterClose(t *testing.T) {
	cfg, trace := runtimeMCPServer(t)
	h := newMCPHost(context.Background(), t.TempDir(), map[string]extensions.MCPServer{"native": cfg}, nil, true)
	h.Close()
	defer h.Close()
	if _, err := h.Execute(context.Background(), runtimeCall("connect_mcp_server", map[string]any{"server": "native"})); err == nil {
		t.Fatal("closed host started another process")
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("closed host started process: %v", err)
	}
}

func TestRedactedArgsHidesCredentialFlagsWithoutChangingExecution(t *testing.T) {
	tests := []struct {
		name       string
		args, want []string
	}{
		{"separate values", []string{"--api-key", "dummy-api-value", "--token", "dummy-token", "--password", "dummy-password", "--client-secret", "dummy-secret"}, []string{"--api-key", "[redacted]", "--token", "[redacted]", "--password", "[redacted]", "--client-secret", "[redacted]"}},
		{"equals values", []string{"--api_key=dummy-api", "--access-token=dummy-token", "--password=dummy-password", "--client-secret=dummy-secret"}, []string{"--api_key=[redacted]", "--access-token=[redacted]", "--password=[redacted]", "--client-secret=[redacted]"}},
		{"case insensitive", []string{"--API-KEY=DUMMY", "--PASSWORD", "dummy"}, []string{"--API-KEY=[redacted]", "--PASSWORD", "[redacted]"}},
		{"ordinary flags", []string{"--port", "3107", "--cwd=/workspace", "serve"}, []string{"--port", "3107", "--cwd=/workspace", "serve"}},
		{"no value", []string{"--token"}, []string{"--token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			got := redactedArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("redacted arguments: got %q want %q", got, tt.want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatal("redaction changed actual process arguments")
			}
		})
	}
	if got := redactedArgs(nil); got != nil {
		t.Fatalf("nil args changed: %v", got)
	}
}
