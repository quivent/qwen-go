// Package builtinmcp provides the small native Git plugin shipped with qwen-go.
package builtinmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Serve implements the MCP stdio lifecycle without requiring a separate runtime.
func Serve(ctx context.Context, in io.Reader, out io.Writer, cwd string) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	encoder := json.NewEncoder(out)
	for scan.Scan() {
		var r request
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			return fmt.Errorf("invalid MCP request: %w", err)
		}
		if len(r.ID) == 0 {
			continue
		}
		var result any
		var rpcErr any
		switch r.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "qwen-go-workspace", "version": "0.2.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "git_status", "description": "Show working tree and branch status without changing repository files.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
				map[string]any{"name": "git_diff", "description": "Show unstaged or staged changes. External diff drivers, text conversion and configured filters are disabled; submodules are ignored.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"staged": map[string]string{"type": "boolean"}}, "additionalProperties": false}},
			}}
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments struct {
					Staged bool `json:"staged"`
				} `json:"arguments"`
			}
			if err := json.Unmarshal(r.Params, &params); err != nil {
				rpcErr = map[string]any{"code": -32602, "message": "Invalid arguments"}
				break
			}
			var toolArgs []string
			switch params.Name {
			case "git_status":
				toolArgs = []string{"status", "--short", "--branch", "--ignore-submodules=all"}
			case "git_diff":
				toolArgs = []string{"diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all"}
				if params.Arguments.Staged {
					toolArgs = append(toolArgs, "--cached")
				}
			default:
				rpcErr = map[string]any{"code": -32602, "message": "Unknown tool"}
			}
			if rpcErr != nil {
				break
			}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			args, err := readOnlyGitOptions(callCtx, cwd)
			var output cappedBuffer
			if err == nil {
				args = append(args, toolArgs...)
				cmd := exec.CommandContext(callCtx, "git", args...)
				cmd.Dir = cwd
				cmd.Stdout = &output
				cmd.Stderr = &output
				err = cmd.Run()
			}
			cancel()
			text := output.String()
			if output.truncated {
				text += "\n[Output truncated at 64 KiB]"
			}
			if err != nil {
				text += "\n" + err.Error()
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": err != nil}
		default:
			rpcErr = map[string]any{"code": -32601, "message": "Method not found"}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": r.ID}
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			response["result"] = result
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return scan.Err()
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Len() int       { return b.buffer.Len() }
func (b *cappedBuffer) String() string { return b.buffer.String() }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 65536 - b.Len()
	if len(p) > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

// A read-only diff can otherwise execute .gitattributes clean/process filters.
// Enumerate only config key NAMES (never secret-valued commands) and explicitly
// override every configured filter. Enumeration is bounded and failures close the
// operation instead of falling back to potentially executable Git helpers.
var filterNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func readOnlyGitOptions(ctx context.Context, cwd string) ([]string, error) {
	options := []string{"-c", "core.fsmonitor=false", "--no-optional-locks"}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	query := exec.CommandContext(bounded, "git", "-c", "core.fsmonitor=false", "--no-optional-locks", "config", "--includes", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
	query.Dir = cwd
	var names cappedBuffer
	query.Stdout = &names
	query.Stderr = io.Discard
	err := query.Run()
	if bounded.Err() != nil {
		return nil, fmt.Errorf("Git filter inspection interrupted: %w", bounded.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, errors.New("cannot safely inspect Git filter configuration")
		}
	}
	if names.truncated {
		return nil, errors.New("Git filter configuration exceeds inspection limit")
	}
	filters := map[string]bool{}
	for _, key := range strings.Split(names.String(), "\x00") {
		if key == "" {
			continue
		}
		if !strings.HasPrefix(key, "filter.") {
			return nil, errors.New("invalid Git filter configuration key")
		}
		field := strings.LastIndexByte(key, '.')
		if field <= len("filter.") {
			return nil, errors.New("invalid Git filter configuration key")
		}
		name := key[len("filter."):field]
		if !filterNamePattern.MatchString(name) {
			return nil, errors.New("unsupported Git filter name; refusing to run external helpers")
		}
		filters[name] = true
		if len(filters) > 128 {
			return nil, errors.New("too many configured Git filters")
		}
	}
	ordered := make([]string, 0, len(filters))
	for name := range filters {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		for _, field := range []string{"clean", "process", "smudge"} {
			options = append(options, "-c", "filter."+name+"."+field+"=")
		}
		options = append(options, "-c", "filter."+name+".required=false")
	}
	return options, nil
}
