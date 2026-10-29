package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"qwen-go/internal/agents"
	"qwen-go/internal/builtinmcp"
	"qwen-go/internal/core"
	"qwen-go/internal/extensions"
	"qwen-go/internal/provider"
	"qwen-go/internal/toolkit"
	"qwen-go/internal/tui"
)

const version = "0.2.0"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "qwen-go:", terminalText(err.Error()))
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	store, err := core.SessionsDir()
	if err != nil {
		return err
	}
	if override := os.Getenv("QWEN_GO_STATE_DIR"); override != "" {
		store = override
	}
	if len(args) > 0 {
		switch args[0] {
		case "mcp-serve":
			cwd, e := os.Getwd()
			if e != nil {
				return e
			}
			return builtinmcp.Serve(ctx, os.Stdin, out, cwd)
		case "login":
			return provider.Login(ctx, out)
		case "auth":
			if len(args) > 1 && args[1] == "login" {
				return provider.Login(ctx, out)
			}
			return fmt.Errorf("usage: qwen-go auth login")
		case "sessions":
			all, e := core.ListSessions(store)
			if e != nil {
				return e
			}
			if len(all) == 0 {
				fmt.Fprintln(out, "No saved sessions.")
				return nil
			}
			for _, s := range all {
				kind := "chat"
				if s.ParentID != "" {
					kind = "agent"
				}
				fmt.Fprintf(out, "%s  %s  %-5s  %s  %s\n", s.ID, s.Updated.Local().Format("Jan 02 15:04"), kind, terminalText(s.Model), terminalText(s.Title))
			}
			return nil
		case "version":
			fmt.Fprintf(out, "qwen-go %s · native Go · no Node/V8\n", version)
			return nil
		}
	}
	listPlugins := false
	if len(args) >= 2 && args[0] == "plugins" && args[1] == "list" {
		listPlugins = true
		args = args[2:]
	}
	flags := flag.NewFlagSet("qwen-go", flag.ContinueOnError)
	flags.SetOutput(errOut)
	var model, baseURL, cwd, prompt, resume string
	var demo, yes, jsonEvents, showVersion bool
	var pluginDirs stringList
	var maxAgents int
	flags.StringVar(&model, "model", "", "model name (or QWEN_MODEL / OPENAI_MODEL)")
	flags.StringVar(&baseURL, "base-url", "", "OpenAI-compatible API base URL")
	flags.StringVar(&cwd, "cwd", "", "workspace directory (default current directory)")
	flags.StringVar(&prompt, "prompt", "", "run one prompt without opening the TUI")
	flags.StringVar(&prompt, "p", "", "alias for --prompt")
	flags.StringVar(&resume, "resume", "", "resume a session ID or latest")
	flags.BoolVar(&demo, "demo", false, "offline interface demonstration; no model calls")
	flags.BoolVar(&yes, "yes", false, "approve ALL mutations, shell commands, write agents, and MCP startup/tool calls")
	flags.BoolVar(&jsonEvents, "json", false, "emit newline-delimited events with --prompt")
	flags.BoolVar(&showVersion, "version", false, "print version")
	flags.Var(&pluginDirs, "plugin", "additional local plugin directory (repeatable)")
	flags.IntVar(&maxAgents, "agents", 3, "maximum concurrent subagents (1–8)")
	flags.Usage = func() {
		fmt.Fprint(errOut, "Qwen Go — native terminal coding client\n\nUsage: qwen-go [options]\n       qwen-go login | sessions | version\n       qwen-go plugins list [--cwd PATH] [--plugin PATH]\n\nCommands: /help /new /session /model /agents /plugins /skills /quit\n\n")
		flags.PrintDefaults()
	}
	if err = flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if showVersion {
		fmt.Fprintf(out, "qwen-go %s · native Go · no Node/V8\n", version)
		return nil
	}
	if maxAgents < 1 || maxAgents > 8 {
		return fmt.Errorf("--agents must be between 1 and 8")
	}
	if prompt == "" && len(flags.Args()) > 0 {
		prompt = strings.Join(flags.Args(), " ")
	}
	var session *core.Session
	if resume != "" {
		session, err = core.LoadSession(store, resume)
		if err != nil {
			return err
		}
		if model == "" {
			model = session.Model
		}
		if cwd == "" {
			cwd = session.CWD
		}
	}
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory: %s", cwd)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(home, ".config", "qwen-go", "plugins"), filepath.Join(cwd, ".qwen-go", "plugins"), filepath.Join(home, ".qwen", "extensions"), filepath.Join(cwd, ".qwen", "extensions")}
	paths = append(paths, pluginDirs...)
	registry, err := extensions.LoadPaths(cwd, paths)
	if err != nil {
		return fmt.Errorf("load plugins: %w", err)
	}
	if listPlugins {
		fmt.Fprintln(out, terminalText(registry.List()))
		return nil
	}
	var client core.Streamer
	if demo {
		model = "offline-demo"
		client = demoClient{}
	} else {
		cfg, e := provider.LoadConfig(model, baseURL)
		if e != nil {
			return e
		}
		model = cfg.Model
		client = provider.New(cfg)
	}
	if session == nil {
		session = core.NewSession(cwd, model)
	} else {
		session.CWD = cwd
		session.Model = model
	}
	background := make(chan core.Event, 256)
	var outputMu sync.Mutex
	var turnErr error
	headlessEmit := func(event core.Event) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if jsonEvents {
			_ = json.NewEncoder(out).Encode(struct {
				Kind string `json:"kind"`
				Text string `json:"text,omitempty"`
				Name string `json:"name,omitempty"`
			}{event.Kind, event.Text, event.Name})
		} else {
			switch event.Kind {
			case "delta":
				fmt.Fprint(out, terminalText(event.Text))
			case "error":
				fmt.Fprintln(errOut, terminalText(event.Text))
			case "tool":
				fmt.Fprintf(errOut, "[%s] %s\n", terminalText(event.Name), terminalText(event.Text))
			case "info", "agent":
				fmt.Fprintln(errOut, terminalText(event.Text))
			}
		}
		if event.Kind == "error" {
			turnErr = fmt.Errorf("%s", event.Text)
		}
	}
	appEmit := func(event core.Event) {
		select {
		case background <- event:
		case <-ctx.Done():
		}
	}
	var approve func(context.Context, string) bool
	if prompt != "" {
		appEmit = headlessEmit
		approve = func(_ context.Context, description string) bool {
			outputMu.Lock()
			defer outputMu.Unlock()
			fmt.Fprintln(errOut, "Tool denied in non-interactive mode:", terminalText(description), "(use --yes to explicitly allow writes/shell)")
			return false
		}
	} else {
		broker := newApprovalBroker(appEmit)
		approve = broker.approve
	}
	executor := &toolkit.Executor{CWD: cwd, AllowAll: yes, Approve: approve}
	instructions := registry.Instructions() + "\n\nDelegate independent work with spawn_agent. Children have separate context and saved sessions, and default to read-only. Spawn independent tasks before waiting so they run concurrently. Use wait_agent to collect their reports; do not claim their work is complete until they finish. Children cannot delegate recursively. To use configured plugin tools, list_mcp_servers then connect_mcp_server, which requires approval. Each external tool call also requires approval."
	manager := agents.New(ctx, agents.Options{CWD: cwd, StoreDir: store, Model: model, ParentID: session.ID, Client: client, Approve: approve, AllowAll: yes, MaxConcurrent: maxAgents, Emit: appEmit, Instructions: registry.Instructions()})
	runtime := &toolRuntime{base: executor, agents: manager, mcp: newMCPHost(ctx, cwd, registry.MCPServers, approve, yes)}
	defer func() { cancel(); runtime.Close() }()
	runner := &core.Runner{Client: client, Executor: runtime, ToolsFunc: runtime.Definitions, Session: session, StoreDir: store, Instructions: instructions, ExpandPrompt: registry.ExpandCommand}
	if prompt != "" {
		if !extensionCommand(registry, manager, prompt, headlessEmit) {
			runner.Run(ctx, prompt, headlessEmit)
		}
		if !jsonEvents {
			fmt.Fprintln(out)
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		return turnErr
	}
	submit := func(turnCtx context.Context, input string, emit func(core.Event)) {
		manager.SetParentID(runner.Session.ID)
		if extensionCommand(registry, manager, input, emit) {
			emit(core.Event{Kind: "done"})
			return
		}
		runner.Run(turnCtx, input, emit)
		if turnCtx.Err() != nil {
			manager.CancelAll()
		}
	}
	return tui.Run(ctx, tui.Config{Model: model, CWD: cwd, SessionID: session.ID, History: session.Messages, Events: background}, submit)
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

type demoClient struct{}

func terminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func (demoClient) Stream(ctx context.Context, messages []core.Message, _ []core.ToolDefinition, delta func(string)) (core.Message, error) {
	text := "This interface is running as a native Go executable. No Node.js or V8 is involved.\n\nStreaming, conversation history, scrolling, and permission prompts are handled in Go. To connect a real model, run `qwen-go login` or configure QWEN_API_KEY / OPENAI_API_KEY.\n\nThis is an offline demonstration; no files were changed and no model was contacted."
	var b strings.Builder
	for _, word := range strings.SplitAfter(text, " ") {
		select {
		case <-ctx.Done():
			return core.Message{}, ctx.Err()
		case <-time.After(12 * time.Millisecond):
		}
		delta(word)
		b.WriteString(word)
	}
	return core.Message{Role: "assistant", Content: b.String()}, nil
}
