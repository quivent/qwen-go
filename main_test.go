package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qwen-go/internal/core"
)

func TestCLIResumeRestoresWorkspace(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	t.Setenv("QWEN_GO_STATE_DIR", state)
	s := core.NewSession(workspace, "offline-demo")
	if err := core.SaveSession(state, s); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if err := run([]string{"--demo", "--resume", s.ID, "--prompt", "continue"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadSession(state, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CWD != workspace {
		t.Fatalf("resumed wrong workspace: %q, wanted %q", loaded.CWD, workspace)
	}
	if !strings.Contains(out.String(), "native Go") {
		t.Fatal("demo did not stream")
	}
	if err := run([]string{"sessions"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), s.ID) {
		t.Fatal("session not listed")
	}
}

func TestCLIWriteRequiresExplicitYes(t *testing.T) {
	t.Setenv("QWEN_GO_STATE_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("QWEN_API_KEY", "")
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []core.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		if last.Role == "user" {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"write1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"created.txt\\\",\\\"content\\\":\\\"hello\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Finished.\"}}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	args := []string{"--base-url", server.URL + "/v1", "--cwd", workspace, "--prompt", "write a file"}
	var out, errs bytes.Buffer
	if err := run(args, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(err) {
		t.Fatal("unapproved file write occurred")
	}
	if !strings.Contains(errs.String(), "Tool denied") {
		t.Fatal("missing denial notice")
	}
	if err := run(append([]string{"--yes"}, args...), &out, &errs); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "created.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("approved write failed: %q %v", data, err)
	}
}

func TestCLIHelpDoesNotRequireAuth(t *testing.T) {
	var out, errs bytes.Buffer
	if err := run([]string{"--help"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs.String(), "Qwen Go") {
		t.Fatal("missing help")
	}
}

func TestHeadlessOutputStripsTerminalControls(t *testing.T) {
	value := terminalText("hello\x1b]52;c;YXR0YWNr\x07\x1b[31mworld\x1b[0m\u009b\n")
	if value != "helloworld\n" {
		t.Fatalf("unsafe terminal text: %q", value)
	}
}
