// Package tui renders the native terminal client. It has no JavaScript runtime.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"qwen-go/internal/core"
)

// Config describes the current workspace and previously saved conversation.
type Config struct {
	Model     string
	CWD       string
	SessionID string
	History   []core.Message
	// Events carries app-level subagent activity and serialized permission prompts.
	Events <-chan core.Event
}

type submission func(context.Context, string, func(core.Event))
type entry struct{ role, text string }
type eventMsg struct {
	id    int
	event core.Event
}
type tickMsg time.Time

type model struct {
	ctx           context.Context
	cfg           Config
	submit        submission
	events        chan eventMsg
	width, height int
	entries       []entry
	input         []rune
	cursor        int
	busy          bool
	cancel        context.CancelFunc
	turnID        int
	assistant     int
	scroll        int
	status        string
	pending       *core.Event
	pendingGlobal bool
	frame         int
	started       time.Time
}

var (
	cyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	muted  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	bright = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	amber  = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	code   = lipgloss.NewStyle().Foreground(lipgloss.Color("152"))
)

// Run owns the terminal until the user quits or ctx is canceled. submit is called
// in a worker goroutine; its events are delivered through Bubble Tea's update loop.
func Run(ctx context.Context, cfg Config, submit func(context.Context, string, func(core.Event))) error {
	m := newModel(ctx, cfg, submit)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	defer func() {
		if m.cancel != nil {
			m.cancel()
		}
	}()
	final, err := p.Run()
	if state, ok := final.(*model); ok {
		state.rejectApproval()
		if state.cancel != nil {
			state.cancel()
		}
	}
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func newModel(ctx context.Context, cfg Config, submit submission) *model {
	m := &model{ctx: ctx, cfg: cfg, submit: submit, events: make(chan eventMsg, 256), width: 80, height: 24, assistant: -1, status: "Ready", started: time.Now()}
	for _, msg := range cfg.History {
		if msg.Role == "system" || msg.Content == "" {
			continue
		}
		m.entries = append(m.entries, entry{role: msg.Role, text: msg.Content})
	}
	return m
}

func (m *model) Init() tea.Cmd { return m.waitEvent() }
func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *model) waitEvent() tea.Cmd {
	appEvents := m.cfg.Events
	return func() tea.Msg {
		for {
			select {
			case e := <-m.events:
				return e
			case e, ok := <-appEvents:
				if !ok {
					appEvents = nil
					continue
				}
				return eventMsg{-1, e}
			case <-m.ctx.Done():
				return tea.Quit()
			}
		}
	}
}
func (m *model) rejectApproval() {
	if m.pending != nil && m.pending.Reply != nil {
		select {
		case m.pending.Reply <- false:
		default:
		}
	}
	m.pending = nil
	m.pendingGlobal = false
}
func (m *model) answerApproval(yes bool) {
	if m.pending == nil {
		return
	}
	if m.pending.Reply != nil {
		select {
		case m.pending.Reply <- yes:
		default:
		}
	}
	m.pending = nil
	m.pendingGlobal = false
	if yes {
		m.status = "Approved · executing"
	} else {
		m.status = "Declined"
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = max(20, msg.Width)
		m.height = max(10, msg.Height)
	case tickMsg:
		if m.busy {
			m.frame++
			return m, tick()
		}
		return m, nil
	case eventMsg:
		if msg.id == m.turnID || msg.id == -1 {
			if msg.event.Kind == "approval" {
				m.pendingGlobal = msg.id == -1
			}
			m.handleEvent(msg.event)
		}
		return m, m.waitEvent()
	case tea.KeyMsg:
		if m.pending != nil {
			switch msg.String() {
			case "y", "Y":
				m.answerApproval(true)
			case "pgup", "alt+up":
				m.scroll += max(1, m.chatHeight()/2)
			case "pgdown", "alt+down":
				m.scroll = max(0, m.scroll-max(1, m.chatHeight()/2))
			case "ctrl+home":
				m.scroll = 1 << 30
			case "ctrl+end":
				m.scroll = 0
			case "n", "N", "esc", "enter":
				m.answerApproval(false)
			case "ctrl+c":
				m.rejectApproval()
				if m.cancel != nil {
					m.cancel()
				}
				m.status = "Canceling…"
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			if m.busy {
				if m.cancel != nil {
					m.cancel()
				}
				m.status = "Canceling…"
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+d":
			if len(m.input) == 0 && !m.busy {
				return m, tea.Quit
			}
			if m.cursor < len(m.input) {
				m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
			}
		case "enter":
			if m.busy {
				return m, nil
			}
			text := strings.TrimSpace(string(m.input))
			if text == "" {
				return m, nil
			}
			m.input = nil
			m.cursor = 0
			switch text {
			case "/quit", "/exit":
				return m, tea.Quit
			case "/help":
				m.entries = append(m.entries, entry{"system", helpText})
				m.scroll = 0
				return m, nil
			case "/clear":
				m.entries = nil
				m.scroll = 0
				return m, nil
			case "/new":
				m.entries = nil
				m.scroll = 0
			default:
				m.entries = append(m.entries, entry{"user", text})
			}
			m.begin(text)
			return m, tick()
		case "alt+enter", "ctrl+j":
			m.insert([]rune{'\n'})
		case "backspace", "ctrl+h":
			if m.cursor > 0 {
				m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
				m.cursor--
			}
		case "delete":
			if m.cursor < len(m.input) {
				m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
			}
		case "left":
			if m.cursor > 0 {
				m.cursor--
			}
		case "right":
			if m.cursor < len(m.input) {
				m.cursor++
			}
		case "home", "ctrl+a":
			m.cursor = lineStart(m.input, m.cursor)
		case "end", "ctrl+e":
			m.cursor = lineEnd(m.input, m.cursor)
		case "up":
			m.moveVertical(-1)
		case "down":
			m.moveVertical(1)
		case "ctrl+u":
			start := lineStart(m.input, m.cursor)
			m.input = append(m.input[:start], m.input[m.cursor:]...)
			m.cursor = start
		case "ctrl+k":
			end := lineEnd(m.input, m.cursor)
			m.input = append(m.input[:m.cursor], m.input[end:]...)
		case "ctrl+w":
			start := m.cursor
			for start > 0 && unicode.IsSpace(m.input[start-1]) {
				start--
			}
			for start > 0 && !unicode.IsSpace(m.input[start-1]) {
				start--
			}
			m.input = append(m.input[:start], m.input[m.cursor:]...)
			m.cursor = start
		case "pgup", "alt+up":
			m.scroll += max(1, m.chatHeight()/2)
		case "pgdown", "alt+down":
			m.scroll = max(0, m.scroll-max(1, m.chatHeight()/2))
		case "ctrl+home":
			m.scroll = 1 << 30
		case "ctrl+end":
			m.scroll = 0
		case "ctrl+l":
			m.entries = nil
			m.assistant = -1
			m.scroll = 0
		case " ":
			m.insert([]rune{' '})
		case "tab":
			m.insert([]rune("  "))
		default:
			if msg.Type == tea.KeyRunes {
				var chars []rune
				for _, r := range msg.Runes {
					if r == '\n' || r == '\t' || r >= 32 && r != 127 {
						chars = append(chars, r)
					}
				}
				m.insert(chars)
			}
		}
	}
	return m, nil
}

func (m *model) insert(chars []rune) {
	if len(chars) == 0 {
		return
	}
	tail := append([]rune(nil), m.input[m.cursor:]...)
	m.input = append(m.input[:m.cursor], chars...)
	m.input = append(m.input, tail...)
	m.cursor += len(chars)
}
func lineStart(s []rune, pos int) int {
	for pos > 0 && s[pos-1] != '\n' {
		pos--
	}
	return pos
}
func lineEnd(s []rune, pos int) int {
	for pos < len(s) && s[pos] != '\n' {
		pos++
	}
	return pos
}
func (m *model) moveVertical(direction int) {
	start := lineStart(m.input, m.cursor)
	column := m.cursor - start
	if direction < 0 {
		if start == 0 {
			return
		}
		previous := lineStart(m.input, start-1)
		m.cursor = min(previous+column, start-1)
	} else {
		end := lineEnd(m.input, m.cursor)
		if end == len(m.input) {
			return
		}
		m.cursor = min(end+1+column, lineEnd(m.input, end+1))
	}
}
func (m *model) begin(text string) {
	m.busy = true
	m.status = "Thinking"
	m.assistant = -1
	m.scroll = 0
	m.turnID++
	id := m.turnID
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				m.sendEvent(id, core.Event{Kind: "error", Text: fmt.Sprintf("Request failed: %v", failure)})
			}
			m.sendEvent(id, core.Event{Kind: "done"})
			cancel()
		}()
		m.submit(ctx, text, func(event core.Event) {
			select {
			case m.events <- eventMsg{id, event}:
			case <-ctx.Done():
			}
		})
	}()
}
func (m *model) sendEvent(id int, e core.Event) {
	select {
	case m.events <- eventMsg{id, e}:
	case <-m.ctx.Done():
	}
}
func (m *model) handleEvent(e core.Event) {
	switch e.Kind {
	case "reset":
		m.entries = nil
		m.assistant = -1
		m.scroll = 0
		if e.Text != "" {
			m.cfg.SessionID = e.Text
		}
	case "delta":
		if m.assistant < 0 {
			m.entries = append(m.entries, entry{role: "assistant"})
			m.assistant = len(m.entries) - 1
		}
		m.entries[m.assistant].text += e.Text
		m.status = "Responding"
	case "status":
		m.status = e.Text
		if strings.HasPrefix(e.Text, "Session:") || strings.HasPrefix(e.Text, "Model:") || strings.HasPrefix(e.Text, "Commands:") {
			m.entries = append(m.entries, entry{role: "system", text: e.Text})
		}
	case "agent", "info":
		m.entries = append(m.entries, entry{role: "system", text: e.Text})
		m.assistant = -1
		m.status = strings.SplitN(e.Text, "\n", 2)[0]
	case "tool":
		title := e.Name
		if title == "" {
			title = "Tool"
		}
		body := e.Text
		if len(body) > 8000 {
			body = body[:8000] + "\n… output truncated in view"
		}
		m.entries = append(m.entries, entry{role: "tool", text: title + "\n" + body})
		m.assistant = -1
		m.status = title
	case "error":
		m.entries = append(m.entries, entry{role: "error", text: e.Text})
		m.assistant = -1
		m.status = "Request failed"
	case "approval":
		m.pending = &e
		m.entries = append(m.entries, entry{role: "approval", text: e.Text})
		m.assistant = -1
		m.scroll = 0
		m.status = "Approval required"
	case "approval_cancel":
		if m.pending != nil && m.pending.Reply == e.Reply {
			m.rejectApproval()
			if m.busy {
				m.status = "Thinking…"
			} else {
				m.status = "Ready"
			}
		}
	case "done":
		m.busy = false
		if !m.pendingGlobal {
			m.rejectApproval()
		}
		m.assistant = -1
		if m.pending != nil {
			m.status = "Approval required"
		} else if m.status != "Request failed" {
			m.status = "Ready"
		}
	}
}

const helpText = "Enter sends · Alt+Enter adds a line · PgUp/PgDn scroll · Ctrl+C cancels a request or exits when idle · Ctrl+D exits with an empty prompt.\n/help · /new · /clear · /session · /model · /agents · /plugins · /skills · /quit.\nUse /plugin:command or /skill plugin:skill for installed extensions.\nTool approval: Y allows once; N, Enter or Escape declines."

// clean makes provider/tool text safe to display without letting remote text
// write terminal escape sequences, change the title, or manipulate the cursor.
func clean(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}
func (m *model) chatHeight() int { return max(1, m.height-11) }
func (m *model) renderChat() []string {
	width := max(10, m.width-4)
	if len(m.entries) == 0 {
		text := "What would you like to build?\n\nRead and edit files, search your workspace, and run commands with approval.\nType /help for keyboard shortcuts."
		lines := strings.Split(ansi.Wrap(text, width, ""), "\n")
		for i := range lines {
			lines[i] = "  " + muted.Render(lines[i])
		}
		return lines
	}
	var out []string
	for _, item := range m.entries {
		label := "Qwen"
		style := cyan
		switch item.role {
		case "user":
			label = "You"
			style = green
		case "tool":
			label = "Tool"
			style = amber
		case "approval":
			label = "Approval requested · PgUp/PgDn to inspect"
			style = amber
		case "error":
			label = "Error"
			style = red
		case "system":
			label = "Info"
			style = muted
		}
		out = append(out, "  "+style.Bold(true).Render(ansi.Truncate(label, width, "…")))
		body := clean(item.text)
		if body == "" && m.busy {
			body = "…"
		}
		codeBlock := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				codeBlock = !codeBlock
				out = append(out, "  "+muted.Render(line))
				continue
			}
			for _, part := range strings.Split(ansi.Wrap(strings.ReplaceAll(line, "\t", "    "), width, ""), "\n") {
				rendered := part
				if codeBlock {
					rendered = code.Render(part)
				} else if strings.HasPrefix(part, "#") {
					rendered = bright.Bold(true).Render(part)
				}
				out = append(out, "  "+rendered)
			}
		}
		out = append(out, "")
	}
	return out
}
func (m *model) View() string {
	width := max(20, m.width)
	lineWidth := max(1, width-2)
	title := cyan.Bold(true).Render("QWEN GO") + "  " + muted.Render("native terminal client")
	header := " " + ansi.Truncate(title, width-2, "…") + "\n " + muted.Render(strings.Repeat("─", lineWidth)) + "\n"
	lines := m.renderChat()
	height := m.chatHeight()
	maxScroll := max(0, len(lines)-height)
	scroll := min(m.scroll, maxScroll)
	end := len(lines) - scroll
	start := max(0, end-height)
	visible := append([]string(nil), lines[start:end]...)
	for len(visible) < height {
		visible = append(visible, "")
	}
	for i, line := range visible {
		visible[i] = ansi.Truncate(line, width, "")
	}
	status := clean(m.status)
	if m.busy {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		status = frames[m.frame%len(frames)] + " " + status + " · Ctrl+C to cancel"
	}
	if scroll > 0 {
		status += fmt.Sprintf(" · %d lines above latest", scroll)
	}
	statusLine := " " + muted.Render(ansi.Truncate(status, width-2, "…")) + "\n"
	var input string
	if m.pending != nil {
		title := "Allow " + clean(m.pending.Name) + "?"
		if m.pending.Name == "" {
			title = "Allow this tool?"
		}
		detail := strings.ReplaceAll(clean(m.pending.Text), "\n", " ")
		input = amber.Render(" "+ansi.Truncate(title, width-2, "…")) + "\n " + ansi.Truncate(detail, width-2, "…") + "\n " + amber.Bold(true).Render(ansi.Truncate("[y] Allow once  [n] Decline  PgUp/PgDn inspect", width-2, "…")) + "\n"
	} else {
		before := clean(string(m.input[:m.cursor]))
		after := clean(string(m.input[m.cursor:]))
		cursor := lipgloss.NewStyle().Reverse(true).Render(" ")
		if len(after) > 0 {
			rs := []rune(after)
			if rs[0] != '\n' {
				cursor = lipgloss.NewStyle().Reverse(true).Render(string(rs[0]))
				after = string(rs[1:])
			}
		}
		text := before + cursor + after
		if len(m.input) == 0 {
			text = cursor + muted.Render(" Ask a question or describe a change…")
		}
		wrapped := strings.Split(ansi.Wrap(text, max(10, width-6), ""), "\n")
		if len(wrapped) > 3 {
			cursorRow := strings.Count(ansi.Wrap(before+cursor, max(10, width-6), ""), "\n")
			start := min(max(0, cursorRow-2), len(wrapped)-3)
			wrapped = wrapped[start : start+3]
		}
		for len(wrapped) < 3 {
			wrapped = append(wrapped, "")
		}
		for i := range wrapped {
			prefix := "   "
			if i == 0 {
				prefix = cyan.Render(" ❯ ")
			}
			wrapped[i] = prefix + wrapped[i]
		}
		input = strings.Join(wrapped, "\n") + "\n"
	}
	meta := m.cfg.Model + "  ·  " + m.cfg.CWD
	if m.cfg.SessionID != "" {
		id := m.cfg.SessionID
		if len(id) > 8 {
			id = id[len(id)-8:]
		}
		meta += "  ·  " + id
	}
	footer := " " + muted.Render(strings.Repeat("─", lineWidth)) + "\n " + muted.Render(ansi.Truncate(clean(meta), width-2, "…")) + "\n " + muted.Render(ansi.Truncate("Enter send · Alt+Enter newline · PgUp/PgDn scroll · /help", width-2, "…"))
	return header + strings.Join(visible, "\n") + "\n" + statusLine + input + footer
}
