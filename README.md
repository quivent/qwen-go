# Qwen Go

A native Go terminal coding client, implemented from Qwen Code's model/tool workflow and terminal interaction patterns. The executable does not contain or invoke Node.js, V8, or JavaScript. This is an independent implementation, not the official Qwen Code release.

## Run

```sh
qwen-go login                  # Native Qwen device authorization; opens no browser automatically
qwen-go --cwd ~/your-project    # Interactive terminal client
qwen-go --demo                 # Offline UI demonstration (no model calls)
qwen-go --prompt 'Explain this repository' --cwd ~/your-project
qwen-go sessions
qwen-go --resume latest
qwen-go plugins list
```

Alternatively configure an API key in your shell (`QWEN_API_KEY` or `OPENAI_API_KEY`). Configure the model with `--model`, `QWEN_MODEL`, or `OPENAI_MODEL`; set a compatible endpoint using `--base-url`, `QWEN_BASE_URL`, or `OPENAI_BASE_URL`. Existing Qwen credentials in `~/.qwen/oauth_creds.json` can be reused. Native login stores its own credentials in `~/.config/qwen-go/oauth.json`, with owner-only permissions. No keys are stored in conversation files.

Without credentials, `--demo`, `--help`, and `sessions` still work. Localhost inference endpoints may omit an API key. A real authenticated model request has not been verified on this machine because no Qwen/API credentials were available during implementation.

## Controls

- Enter: send. Alt+Enter or Ctrl+J: insert a newline.
- Arrow keys, Home/End, Ctrl+A/E/U/K/W: edit input.
- PageUp/PageDown: scroll conversation. Ctrl+Home/End: start/end.
- Ctrl+C: cancel the current turn; when idle, exit. Ctrl+D: exit with empty input.
- `y` or `n`: approve or deny a pending tool action. Approval details can be scrolled.
- `/help`, `/new`, `/clear`, `/session`, `/model`, `/agents`, `/plugins`, `/skills`, `/quit`.

`/clear` clears the display; `/new` starts a new persisted conversation. Sessions are saved atomically at `~/.local/state/qwen-go/sessions/`, including completed tool-call batches. `--resume ID` reopens one and restores its workspace unless `--cwd` is supplied.

## Coding tools

The client implements file read, directory listing, regular-expression search, exact-text editing, file writing, and shell commands. File tools reject workspace traversal and symlink escapes. Workspace `AGENTS.md` is supplied to the model. Nested instruction files should be read before editing their directories.

Writes and shell commands require approval. In noninteractive `--prompt` mode, these actions are denied unless `--yes` is explicitly supplied. **`--yes` approves all mutations and shell commands for that invocation.** Shell execution is not sandboxed: approved commands run with your normal user permissions, with a 120-second limit and process-group cancellation. File content and shell output have size caps. Remote text is stripped of terminal control sequences before display in the TUI.

## Subagents

The model can delegate independent work using `spawn_agent`, `wait_agent`, `list_agents`, `send_agent_message`, and `cancel_agent`. Three children may run concurrently by default; choose `--agents 1` through `--agents 8`. Additional children queue. Every child has a fresh conversation, its own saved transcript and a parent-session ID. Finished children accept followup tasks. `/agents` shows current application jobs; `sessions` identifies saved agent transcripts. `--resume latest` selects a parent conversation, not a child.

Children default to read-only tools, enforced even if a model invents a write call. Write-mode delegation asks permission, and individual writes/shell commands still ask permission unless `--yes` was explicitly supplied. Children share the workspace, so give writers separate files. Recursive delegation is disabled. Cancellation stops active child requests and shell processes; application exit cancels and joins all children. Background activity and permission requests remain visible after the parent finishes a turn.

## Plugins and MCP

The bundled **workspace-tools** plugin supplies `/workspace-tools:review` (parallel independent read-only reviews), `/workspace-tools:test-plan`, and the `evidence-review` skill. It also exposes native Git status/diff tools through MCP, served by this same Go executable with `qwen-go mcp-serve`.

```sh
qwen-go plugins list
qwen-go --plugin /path/to/plugin
# Inside the TUI:
# /workspace-tools:review authentication changes
# /skill workspace-tools:evidence-review the current diff
```

Plugin directories: `~/.config/qwen-go/plugins/`, `<workspace>/.qwen-go/plugins/`, and legacy `~/.qwen/extensions/` / `<workspace>/.qwen/extensions/`. `--plugin` adds a local plugin or a directory containing plugins. See `examples/workspace-tools/` for the complete working format. Copy that directory into the user plugin directory to install the bundled plugin on another machine.

Supported manifests: `plugin.json`, `qwen-extension.json`, and `gemini-extension.json`. Supported content: Markdown commands, the basic legacy TOML command format, `skills/*/SKILL.md`, and `mcpServers` local stdio definitions. Commands substitute `$ARGUMENTS` or `{{args}}` literally; no shell expansion occurs. Skills load their instructions only when invoked. Arbitrary plugin install scripts, lifecycle hooks, JavaScript UI extensions, and remote HTTP MCP are not executed or supported.

A configured MCP server stays dormant until `connect_mcp_server` is called and you approve its executable/arguments. Its tools appear dynamically after discovery. Each MCP tool invocation also requires approval; `--yes` explicitly bypasses both approvals. MCP servers run with your user permissions, not in a sandbox. The Go MCP client does not require Node, although a third-party server you choose may require its own runtime. The included Git server is native Go.

## Implemented scope

Native terminal rendering, streamed model responses, streamed tool-call assembly, a coding-agent tool loop, tool permissions, session save/resume, Qwen device login and refresh, compatible model APIs, cancellation, scrolling and multiline input.

This port does not reproduce Qwen Code's entire extension ecosystem: remote HTTP MCP, arbitrary lifecycle hooks/JavaScript extensions, browser/voice features, automatic context compaction, multimodal attachments, original Qwen conversation import, and sandbox integrations are not implemented. Long conversations remain bounded by the selected model's context window. It is functional code, not a wrapper around the original JavaScript program.

## Build and verify

```sh
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o qwen-go .
install -m 755 qwen-go ~/.local/bin/qwen-go
```

Go 1.24 or newer. The terminal uses Bubble Tea and Lip Gloss. Protocol, permission, filesystem and session code use Go's standard library. Tests cover the complete streamed model -> approved edit -> tool result -> streamed answer -> persisted/resumed conversation flow using a local HTTP test server, plus OAuth refresh/device exchange, path confinement, shell cancellation and TUI interactions.

The existing `qwen` command and JavaScript installation remain unchanged. See `NOTICE` and `LICENSE` for provenance.
