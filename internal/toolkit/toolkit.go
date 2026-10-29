// Package toolkit provides the native coding tools used by qwen-go.
// File tools stay within the workspace. Approved shell commands are unrestricted
// commands running as the current user, not a security sandbox.
package toolkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"qwen-go/internal/core"
)

const maxFileBytes = 1024 * 1024
const maxOutputBytes = 64 * 1024

// Executor carries the workspace and the interactive approval callback.
type Executor struct {
	CWD      string
	Approve  func(context.Context, string) bool
	AllowAll bool
}

// Definitions describes the wire protocol for the model's function calls.
func Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{
		definition("read_file", "Read a UTF-8 workspace file with line numbers. Files are limited to 1 MiB. offset is the first line (1-based).", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
		definition("list_directory", "List workspace files and directories, optionally recursively. Symlinks are listed but never followed.", `{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"max_entries":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false}`),
		definition("search_files", "Search text files for a Go regular expression. Returns path:line:text; skips .git, node_modules and binary/large files. glob filters filenames.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"max_results":{"type":"integer","minimum":1,"maximum":200}},"required":["pattern"],"additionalProperties":false}`),
		definition("write_file", "Create or replace a UTF-8 file in the workspace. Requires user approval. Creates missing parent directories.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`),
		definition("edit_file", "Replace exact text in a workspace file. Requires approval. The number of occurrences must equal expected_count (default 1); otherwise nothing changes.", `{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"expected_count":{"type":"integer","minimum":1}},"required":["path","old_string","new_string"],"additionalProperties":false}`),
		definition("run_shell", "Run a bash command in the workspace after explicit approval. This is not sandboxed. Output is capped at 64 KiB, timeout at 120 seconds. Cancellation terminates the process group.", `{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":120}},"required":["command"],"additionalProperties":false}`),
	}
}

func definition(name, desc, schema string) core.ToolDefinition {
	return core.ToolDefinition{Type: "function", Function: core.FunctionDefinition{Name: name, Description: desc, Parameters: json.RawMessage(schema)}}
}

type arguments struct {
	Path           string `json:"path"`
	Offset         int    `json:"offset"`
	Limit          int    `json:"limit"`
	Recursive      bool   `json:"recursive"`
	MaxEntries     int    `json:"max_entries"`
	Pattern        string `json:"pattern"`
	Glob           string `json:"glob"`
	MaxResults     int    `json:"max_results"`
	Content        string `json:"content"`
	OldString      string `json:"old_string"`
	NewString      string `json:"new_string"`
	ExpectedCount  int    `json:"expected_count"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (e *Executor) Execute(ctx context.Context, call core.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var a arguments
	if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Function.Arguments), &fields); err != nil || fields == nil {
		return "", errors.New("tool arguments must be a JSON object")
	}
	required := map[string][]string{
		"read_file": {"path"}, "write_file": {"path", "content"},
		"edit_file":    {"path", "old_string", "new_string"},
		"search_files": {"pattern"}, "run_shell": {"command"},
	}
	for _, key := range required[call.Function.Name] {
		value, ok := fields[key]
		value = bytes.TrimSpace(value)
		if !ok || len(value) == 0 || value[0] != '"' {
			return "", fmt.Errorf("%s must be supplied as a string", key)
		}
	}
	switch call.Function.Name {
	case "read_file":
		return e.readFile(a)
	case "list_directory":
		return e.listDirectory(ctx, a)
	case "search_files":
		return e.searchFiles(ctx, a)
	case "write_file":
		return e.writeFile(ctx, a)
	case "edit_file":
		return e.editFile(ctx, a)
	case "run_shell":
		return e.runShell(ctx, a)
	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

func (e *Executor) approve(ctx context.Context, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.AllowAll {
		return nil
	}
	if e.Approve == nil || !e.Approve(ctx, prompt) {
		return errors.New("user denied permission; no changes made")
	}
	return ctx.Err()
}

func (e *Executor) workspace() (string, error) {
	cwd := e.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	return root, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolve checks both lexical traversal and symlinks, including an existing
// ancestor when the requested target does not exist yet.
func (e *Executor) resolve(path string) (string, error) {
	root, err := e.workspace()
	if err != nil {
		return "", err
	}
	if path == "" {
		path = "."
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	if !within(root, p) {
		return "", errors.New("path is outside workspace")
	}
	ancestor := p
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			if !within(root, resolved) {
				return "", errors.New("symlink resolves outside workspace")
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		// A dangling symlink must not be treated as a missing directory.
		if info, lerr := os.Lstat(ancestor); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("dangling symlink in path")
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
}

func readText(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, errors.New("file exceeds 1 MiB; use a more targeted shell command")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("binary files are not supported by this text tool")
	}
	return data, nil
}

func (e *Executor) readFile(a arguments) (string, error) {
	if a.Path == "" {
		return "", errors.New("path is required")
	}
	path, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := readText(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] == '\n' {
		lines = lines[:len(lines)-1]
	}
	if a.Offset == 0 {
		a.Offset = 1
	}
	if a.Offset < 1 {
		return "", errors.New("offset must be at least 1")
	}
	if a.Limit == 0 {
		a.Limit = 2000
	}
	if a.Limit < 1 {
		return "", errors.New("limit must be positive")
	}
	start := min(a.Offset-1, len(lines))
	end := min(start+a.Limit, len(lines))
	var out strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&out, "%6d | %s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&out, "[Showing lines %d-%d of %d]\n", start+1, end, len(lines))
	}
	return capped(out.String()), nil
}

func (e *Executor) listDirectory(ctx context.Context, a arguments) (string, error) {
	path, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	limit := a.MaxEntries
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 1000)
	var rows []string
	truncated := false
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == path {
			return nil
		}
		if len(rows) >= limit {
			truncated = true
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(path, p)
		suffix := ""
		if d.IsDir() {
			suffix = "/"
		}
		if d.Type()&os.ModeSymlink != 0 {
			suffix = " -> [symlink]"
		}
		rows = append(rows, rel+suffix)
		if d.IsDir() && !a.Recursive {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(rows)
	out := strings.Join(rows, "\n")
	if truncated {
		out += "\n[Listing truncated]"
	}
	if out == "" {
		out = "[Empty directory]"
	}
	return capped(out), nil
}

func (e *Executor) searchFiles(ctx context.Context, a arguments) (string, error) {
	if a.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regex: %w", err)
	}
	path, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 200)
	root, err := e.workspace()
	if err != nil {
		return "", err
	}
	var rows []string
	visited := 0
	truncated := false
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if p != path && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".venv") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		visited++
		if visited > 20000 {
			truncated = true
			return fs.SkipAll
		}
		if a.Glob != "" {
			matched, err := filepath.Match(a.Glob, d.Name())
			if err != nil {
				return err
			}
			if !matched {
				return nil
			}
		}
		data, err := readText(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				rows = append(rows, fmt.Sprintf("%s:%d:%s", rel, i+1, line))
				if len(rows) >= limit {
					truncated = true
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "No matches found.", nil
	}
	out := strings.Join(rows, "\n")
	if truncated {
		out += "\n[Search truncated]"
	}
	return capped(out), nil
}

func atomicWrite(path string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("path is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".qwen-go-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

func (e *Executor) writeFile(ctx context.Context, a arguments) (string, error) {
	if a.Path == "" {
		return "", errors.New("path is required")
	}
	if len(a.Content) > maxFileBytes {
		return "", errors.New("content exceeds 1 MiB")
	}
	path, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err = e.approve(ctx, fmt.Sprintf("Write %s (%d bytes)", a.Path, len(a.Content))); err != nil {
		return "", err
	}
	// Recheck after the potentially long interactive approval.
	path, err = e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err = atomicWrite(path, []byte(a.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path), nil
}

func (e *Executor) editFile(ctx context.Context, a arguments) (string, error) {
	if a.Path == "" || a.OldString == "" {
		return "", errors.New("path and nonempty old_string are required")
	}
	path, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := readText(path)
	if err != nil {
		return "", err
	}
	expected := a.ExpectedCount
	if expected == 0 {
		expected = 1
	}
	if expected < 1 {
		return "", errors.New("expected_count must be positive")
	}
	count := bytes.Count(data, []byte(a.OldString))
	if count != expected {
		return "", fmt.Errorf("old_string matched %d times; expected %d; nothing changed", count, expected)
	}
	updated := strings.ReplaceAll(string(data), a.OldString, a.NewString)
	if len(updated) > maxFileBytes {
		return "", errors.New("updated file exceeds 1 MiB")
	}
	if err = e.approve(ctx, fmt.Sprintf("Edit %s (%d exact replacements)\n- %s\n+ %s", a.Path, count, capped(a.OldString), capped(a.NewString))); err != nil {
		return "", err
	}
	path, err = e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	now, err := readText(path)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(now, data) {
		return "", errors.New("file changed while awaiting approval; retry with current contents")
	}
	if err = atomicWrite(path, []byte(updated)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Edited %s (%d replacements)", a.Path, count), nil
}

type limitedBuffer struct {
	mu        sync.Mutex
	b         bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := maxOutputBytes - b.b.Len()
	if n > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		_, _ = b.b.Write(p[:min(n, remaining)])
	}
	return n, nil
}
func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.b.String()
	if b.truncated {
		s += "\n[Output truncated at 64 KiB]"
	}
	return s
}
func capped(s string) string {
	if len(s) > maxOutputBytes {
		return s[:maxOutputBytes] + "\n[Output truncated at 64 KiB]"
	}
	return s
}

func (e *Executor) runShell(ctx context.Context, a arguments) (string, error) {
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is required")
	}
	root, err := e.workspace()
	if err != nil {
		return "", err
	}
	if err = e.approve(ctx, "Run shell command (unrestricted, as your user):\n"+a.Command); err != nil {
		return "", err
	}
	seconds := a.TimeoutSeconds
	if seconds <= 0 {
		seconds = 120
	}
	seconds = min(seconds, 120)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", a.Command)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	result := output.String()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, fmt.Errorf("shell failed: %w", err)
	}
	if result == "" {
		result = "[Command completed with no output]"
	}
	return result, nil
}
