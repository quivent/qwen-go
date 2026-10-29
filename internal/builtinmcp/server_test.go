package builtinmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func exchange(t *testing.T, cwd string, messages ...string) []response {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(strings.Join(messages, "\n")+"\n"), &out, cwd); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	var rows []response
	for decoder.More() {
		var row response
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}
func git(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("original line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "sample.txt")
	git(t, root, "commit", "--quiet", "-m", "fixture")
	return root
}
func textResult(t *testing.T, r response) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("JSON-RPC error: %+v", r.Error)
	}
	var value struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &value); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, c := range value.Content {
		text.WriteString(c.Text)
	}
	return text.String(), value.IsError
}
func TestInitializeNotificationAndToolDiscovery(t *testing.T) {
	rows := exchange(t, t.TempDir(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 2 {
		t.Fatalf("notifications must not receive a reply: %+v", rows)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(rows[0].Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2024-11-05" || init.ServerInfo.Name != "qwen-go-workspace" {
		t.Fatalf("bad handshake %+v", init)
	}
	var tools struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rows[1].Result, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 2 || tools.Tools[0].Name != "git_status" || tools.Tools[1].Name != "git_diff" {
		t.Fatalf("bad listing %+v", tools)
	}
}
func TestReadOnlyGitStatusDiffAndStagedDiff(t *testing.T) {
	root := repo(t)
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("staged line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "sample.txt")
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("unstaged line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	rows := exchange(t, root, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_diff","arguments":{}}}`, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_diff","arguments":{"staged":true}}}`)
	if len(rows) != 3 {
		t.Fatalf("responses %d", len(rows))
	}
	status, failed := textResult(t, rows[0])
	if failed || !strings.Contains(status, "sample.txt") {
		t.Fatalf("status %q", status)
	}
	diff, failed := textResult(t, rows[1])
	if failed || !strings.Contains(diff, "+unstaged line") || !strings.Contains(diff, "-staged line") {
		t.Fatalf("diff %q", diff)
	}
	staged, failed := textResult(t, rows[2])
	if failed || !strings.Contains(staged, "+staged line") || !strings.Contains(staged, "-original line") {
		t.Fatalf("staged diff %q", staged)
	}
	after, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only tools changed Git index")
	}
}
func TestExternalDiffAndFSMonitorAreNotRun(t *testing.T) {
	root := repo(t)
	marker := filepath.Join(root, "unexpected-execution")
	script := filepath.Join(root, "unwanted.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "config", "diff.external", script)
	git(t, root, "config", "core.fsmonitor", script)
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows := exchange(t, root, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_diff","arguments":{}}}`)
	for _, r := range rows {
		if _, failed := textResult(t, r); failed {
			t.Fatal("Git fixture failed")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Git invoked external diff or fsmonitor")
	}
}
func TestProtocolErrorsAndGitErrors(t *testing.T) {
	rows := exchange(t, t.TempDir(), `{"jsonrpc":"2.0","id":1,"method":"unknown"}`, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"rm_everything","arguments":{}}}`, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`)
	if len(rows) != 3 || rows[0].Error == nil || rows[0].Error.Code != -32601 || rows[1].Error == nil || rows[1].Error.Code != -32602 {
		t.Fatalf("wrong protocol errors %+v", rows)
	}
	text, failed := textResult(t, rows[2])
	if !failed || !strings.Contains(strings.ToLower(text), "not a git repository") {
		t.Fatalf("Git failure lost: %q", text)
	}
}
func TestOutputIsBounded(t *testing.T) {
	var b cappedBuffer
	input := bytes.Repeat([]byte("x"), 100000)
	n, err := b.Write(input)
	if err != nil || n != len(input) || b.Len() != 65536 {
		t.Fatalf("cap: n=%d len=%d err=%v", n, b.Len(), err)
	}
	n, err = b.Write([]byte("more"))
	if err != nil || n != 4 || b.Len() != 65536 {
		t.Fatal("cap not maintained")
	}
}
func TestMalformedRequestReturnsError(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader("{bad json}\n"), &out, t.TempDir())
	if err == nil {
		t.Fatal("malformed request accepted")
	}
	if out.Len() != 0 {
		t.Fatal(fmt.Sprintf("unexpected malformed response %q", out.String()))
	}
}

func TestConfiguredCleanAndProcessFiltersAreNeverExecuted(t *testing.T) {
	root := repo(t)
	for _, name := range []string{"clean-file.txt", "process-file.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("original\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("clean-file.txt filter=cleaner\nprocess-file.txt filter=processor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "--quiet", "-m", "filter fixture before executable filter config")
	cleanMarker := filepath.Join(root, "clean-executed")
	processMarker := filepath.Join(root, "process-executed")
	for name, marker := range map[string]string{"clean.sh": cleanMarker, "process.sh": processMarker} {
		script := filepath.Join(root, name)
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "config", "filter.cleaner.clean", filepath.Join(root, "clean.sh"))
	git(t, root, "config", "filter.cleaner.required", "true")
	git(t, root, "config", "filter.processor.process", filepath.Join(root, "process.sh"))
	git(t, root, "config", "filter.processor.clean", filepath.Join(root, "clean.sh"))
	git(t, root, "config", "filter.processor.required", "true")
	for _, name := range []string{"clean-file.txt", "process-file.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("modified content\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows := exchange(t, root, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_diff","arguments":{}}}`)
	for _, r := range rows {
		text, failed := textResult(t, r)
		if failed {
			t.Fatalf("hardened Git command failed: %s", text)
		}
	}
	diff, _ := textResult(t, rows[1])
	if !strings.Contains(diff, "+modified content") {
		t.Fatal("Git diff lost actual changes")
	}
	for _, marker := range []string{cleanMarker, processMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("Git unexpectedly executed configured filter: %s", marker)
		}
	}
}

func TestGitFilterInspectionFailsClosedForUnsupportedName(t *testing.T) {
	root := repo(t)
	git(t, root, "config", "filter.strange=name.clean", "echo not-run")
	_, err := readOnlyGitOptions(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "unsupported Git filter name") {
		t.Fatalf("unsafe config name accepted: %v", err)
	}
}

func TestOutputCapCannotBeBypassedByIOCopy(t *testing.T) {
	var b cappedBuffer
	src := io.LimitReader(bytes.NewReader(bytes.Repeat([]byte("x"), 100000)), 100000)
	n, err := io.Copy(&b, src)
	if err != nil || n != 100000 || b.Len() != 65536 || !b.truncated {
		t.Fatalf("io.Copy bypassed output cap: n=%d len=%d truncated=%t err=%v", n, b.Len(), b.truncated, err)
	}
}
