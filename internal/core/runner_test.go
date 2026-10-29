package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qwen-go/internal/core"
	"qwen-go/internal/provider"
	"qwen-go/internal/toolkit"
)

// Verify the complete wire -> model tool call -> approved edit -> wire result
// -> streamed answer -> saved/resumed history flow with an actual HTTP server.
func TestCodingRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	store := t.TempDir()
	target := filepath.Join(workspace, "hello.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("bad path %s", r.URL.Path)
		}
		var body struct {
			Messages []core.Message        `json:"messages"`
			Tools    []core.ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if len(body.Tools) != 6 {
			t.Errorf("expected six tools, got %d", len(body.Tools))
		}
		if requests == 1 {
			call := core.ToolCall{ID: "edit-1", Type: "function", Function: core.FunctionCall{Name: "edit_file", Arguments: `{"path":"hello.txt","old_string":"before","new_string":"after"}`}}
			delta := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": call.ID, "type": "function", "function": call.Function}}}}}}
			b, _ := json.Marshal(delta)
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		} else {
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "edit-1" || strings.HasPrefix(last.Content, "Error:") {
				t.Errorf("bad tool result: %+v", last)
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Updated hello.txt.\"}}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	client := provider.New(provider.Config{BaseURL: server.URL + "/v1", Model: "test", APIKey: "test-not-a-secret"})
	approvals := 0
	executor := &toolkit.Executor{CWD: workspace, Approve: func(context.Context, string) bool { approvals++; return true }}
	s := core.NewSession(workspace, "test")
	runner := &core.Runner{Client: client, Executor: executor, Tools: toolkit.Definitions(), Session: s, StoreDir: store}
	var text strings.Builder
	done := false
	runner.Run(context.Background(), "Change before to after", func(e core.Event) {
		switch e.Kind {
		case "delta":
			text.WriteString(e.Text)
		case "error":
			t.Error(e.Text)
		case "done":
			done = true
		}
	})
	data, _ := os.ReadFile(target)
	if string(data) != "after\n" {
		t.Fatalf("edit did not happen: %q", data)
	}
	if approvals != 1 || requests != 2 || !done || text.String() != "Updated hello.txt." {
		t.Fatalf("approvals=%d requests=%d done=%v text=%q", approvals, requests, done, text.String())
	}
	loaded, err := core.LoadSession(store, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 4 || loaded.Messages[2].Role != "tool" {
		t.Fatalf("bad saved transcript: %+v", loaded.Messages)
	}
	list, err := core.ListSessions(store)
	if err != nil || len(list) != 1 {
		t.Fatalf("session list: %v %v", list, err)
	}
	info, _ := os.Stat(filepath.Join(store, s.ID+".json"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("private session mode %v", info.Mode())
	}
}

func TestSessionRejectsTraversal(t *testing.T) {
	if _, err := core.LoadSession(t.TempDir(), "../outside"); err == nil {
		t.Fatal("accepted traversal")
	}
	s := core.NewSession("/tmp", "test")
	s.ID = "../outside"
	if err := core.SaveSession(t.TempDir(), s); err == nil {
		t.Fatal("accepted unsafe save")
	}
}

func TestLatestSessionSkipsNewerChild(t *testing.T) {
	store := t.TempDir()
	parent := core.NewSession(t.TempDir(), "test")
	if err := core.SaveSession(store, parent); err != nil {
		t.Fatal(err)
	}
	child := core.NewSession(parent.CWD, "test")
	child.ParentID = parent.ID
	if err := core.SaveSession(store, child); err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadSession(store, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != parent.ID {
		t.Fatal("latest resumed a child instead of its parent")
	}
}

type forbiddenModel struct{}

func (forbiddenModel) Stream(context.Context, []core.Message, []core.ToolDefinition, func(string)) (core.Message, error) {
	panic("commands must not call model")
}

func TestNewSessionCommand(t *testing.T) {
	s := core.NewSession(t.TempDir(), "test")
	old := s.ID
	r := &core.Runner{Client: forbiddenModel{}, Session: s, StoreDir: t.TempDir()}
	r.Run(context.Background(), "/new", func(core.Event) {})
	if r.Session.ID == old || len(r.Session.Messages) != 0 {
		t.Fatal("new session not reset")
	}
}

type failedShell struct{}

func (failedShell) Execute(context.Context, core.ToolCall) (string, error) {
	return "compiler: missing closing brace", fmt.Errorf("exit status 1")
}

type shellModel struct{ calls int }

func (m *shellModel) Stream(_ context.Context, messages []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
	m.calls++
	if m.calls == 1 {
		return core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "shell1", Type: "function", Function: core.FunctionCall{Name: "run_shell", Arguments: `{ "command": "go test ./..." }`}}}}, nil
	}
	last := messages[len(messages)-1]
	if !strings.Contains(last.Content, "compiler: missing closing brace") || !strings.Contains(last.Content, "exit status 1") {
		return core.Message{}, fmt.Errorf("failed command diagnostics were discarded: %q", last.Content)
	}
	return core.Message{Role: "assistant", Content: "I can see the compiler error."}, nil
}

func TestFailedShellPreservesDiagnostics(t *testing.T) {
	r := &core.Runner{Client: &shellModel{}, Executor: failedShell{}, Session: core.NewSession(t.TempDir(), "test"), StoreDir: t.TempDir()}
	r.Run(context.Background(), "run the tests", func(e core.Event) {
		if e.Kind == "error" {
			t.Error(e.Text)
		}
	})
	if len(r.Session.Messages) != 4 {
		t.Fatal("tool round did not complete")
	}
}
