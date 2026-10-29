package toolkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qwen-go/internal/core"
)

func call(name string, args map[string]any) core.ToolCall {
	b, _ := json.Marshal(args)
	return core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: string(b)}}
}
func execute(t *testing.T, e *Executor, name string, args map[string]any) string {
	t.Helper()
	s, err := e.Execute(context.Background(), call(name, args))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPathsStayInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "not-yet"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	e := &Executor{CWD: root, AllowAll: true}
	for _, p := range []string{"../secret", filepath.Join(outside, "secret"), "escape/secret", "escape/new/deep/file", "dangling/file"} {
		t.Run(p, func(t *testing.T) {
			if _, err := e.Execute(context.Background(), call("write_file", map[string]any{"path": p, "content": "bad"})); err == nil {
				t.Fatalf("allowed path %s", p)
			}
		})
	}
	if _, err := e.Execute(context.Background(), call("read_file", map[string]any{"path": "escape/secret"})); err == nil {
		t.Fatal("read followed outside symlink")
	}
}

func TestApprovalDeniedChangesNothing(t *testing.T) {
	root := t.TempDir()
	e := &Executor{CWD: root}
	_, err := e.Execute(context.Background(), call("write_file", map[string]any{"path": "new.txt", "content": "hello"}))
	if err == nil {
		t.Fatal("unapproved write succeeded")
	}
	if _, err = os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("denied file exists")
	}
	_, err = e.Execute(context.Background(), call("run_shell", map[string]any{"command": "touch denied"}))
	if err == nil {
		t.Fatal("unapproved shell succeeded")
	}
	if _, err = os.Stat(filepath.Join(root, "denied")); !os.IsNotExist(err) {
		t.Fatal("denied shell ran")
	}
}

func TestReadWriteAndExactEdit(t *testing.T) {
	root := t.TempDir()
	prompts := 0
	e := &Executor{CWD: root, Approve: func(context.Context, string) bool { prompts++; return true }}
	execute(t, e, "write_file", map[string]any{"path": "src/example.txt", "content": "one\ntwo\ntwo\n"})
	got := execute(t, e, "read_file", map[string]any{"path": "src/example.txt", "offset": 2, "limit": 1})
	if !strings.Contains(got, "2 | two") || strings.Contains(got, "1 | one") {
		t.Fatalf("wrong read: %s", got)
	}
	_, err := e.Execute(context.Background(), call("edit_file", map[string]any{"path": "src/example.txt", "old_string": "two", "new_string": "three"}))
	if err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	if prompts != 1 {
		t.Fatalf("ambiguous edit prompted: %d", prompts)
	}
	execute(t, e, "edit_file", map[string]any{"path": "src/example.txt", "old_string": "two", "new_string": "three", "expected_count": 2})
	data, _ := os.ReadFile(filepath.Join(root, "src/example.txt"))
	if string(data) != "one\nthree\nthree\n" {
		t.Fatalf("wrong edited content: %q", data)
	}
	if prompts != 2 {
		t.Fatalf("expected two approvals, got %d", prompts)
	}
}

func TestEditDetectsChangesDuringApproval(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f")
	os.WriteFile(p, []byte("before"), 0644)
	e := &Executor{CWD: root, Approve: func(context.Context, string) bool { os.WriteFile(p, []byte("changed"), 0644); return true }}
	if _, err := e.Execute(context.Background(), call("edit_file", map[string]any{"path": "f", "old_string": "before", "new_string": "after"})); err == nil {
		t.Fatal("overwrote concurrent edit")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "changed" {
		t.Fatal("concurrent edit lost")
	}
}

func TestListAndSearch(t *testing.T) {
	root := t.TempDir()
	e := &Executor{CWD: root, AllowAll: true}
	execute(t, e, "write_file", map[string]any{"path": "src/main.go", "content": "package main\n// find-this\n"})
	execute(t, e, "write_file", map[string]any{"path": "node_modules/ignored.txt", "content": "find-this"})
	listing := execute(t, e, "list_directory", map[string]any{"recursive": true})
	if !strings.Contains(listing, "src/main.go") {
		t.Fatalf("missing file: %s", listing)
	}
	result := execute(t, e, "search_files", map[string]any{"pattern": "find-this", "glob": "*.go"})
	if !strings.Contains(result, "src/main.go:2:") || strings.Contains(result, "ignored") {
		t.Fatalf("wrong search: %s", result)
	}
}

func TestShellCWDAndOutputLimit(t *testing.T) {
	root := t.TempDir()
	e := &Executor{CWD: root, AllowAll: true}
	got := execute(t, e, "run_shell", map[string]any{"command": "pwd"})
	if strings.TrimSpace(got) != root {
		t.Fatalf("wrong CWD %q", got)
	}
	got = execute(t, e, "run_shell", map[string]any{"command": "head -c 80000 /dev/zero | tr '\\0' x"})
	if len(got) > maxOutputBytes+100 || !strings.Contains(got, "truncated") {
		t.Fatalf("output not capped: %d", len(got))
	}
}

func TestShellCancellationKillsProcessGroup(t *testing.T) {
	root := t.TempDir()
	e := &Executor{CWD: root, AllowAll: true}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Execute(ctx, call("run_shell", map[string]any{"command": "sleep 30 & wait"}))
	if err == nil {
		t.Fatal("cancelled command succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation took %s", time.Since(start))
	}
}

func TestToolDefinitionsAreJSON(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != 6 {
		t.Fatalf("tool count %d", len(definitions))
	}
	for _, d := range definitions {
		if !json.Valid(d.Function.Parameters) {
			t.Fatalf("invalid schema for %s", d.Function.Name)
		}
	}
}

func TestMissingMutationArgumentsCannotEraseFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(path, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{CWD: root, AllowAll: true}
	for _, c := range []core.ToolCall{
		call("write_file", map[string]any{"path": "keep.txt"}),
		call("write_file", map[string]any{"path": "keep.txt", "content": nil}),
		call("edit_file", map[string]any{"path": "keep.txt", "old_string": "keep"}),
		call("edit_file", map[string]any{"path": "keep.txt", "old_string": "keep", "new_string": nil}),
	} {
		if _, err := e.Execute(context.Background(), c); err == nil {
			t.Fatalf("accepted missing string argument: %s", c.Function.Arguments)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep" {
			t.Fatalf("original changed: %q %v", data, err)
		}
	}
	// An explicitly provided empty string remains a valid requested deletion.
	execute(t, e, "edit_file", map[string]any{"path": "keep.txt", "old_string": "keep", "new_string": ""})
	data, _ := os.ReadFile(path)
	if len(data) != 0 {
		t.Fatalf("explicit replacement did not run: %q", data)
	}
}
