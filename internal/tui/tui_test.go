package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"qwen-go/internal/core"
)

func TestRenderRespectsTerminalWidthAndStripsUntrustedEscapes(t *testing.T) {
	m := newModel(context.Background(), Config{Model: "test-model", CWD: "/workspace"}, nil)
	m.width = 42
	m.height = 24
	m.entries = []entry{{"assistant", "Hello \x1b]52;c;Y2xpcGJvYXJk\a world\x1b[2J\n" + strings.Repeat("wide界", 25)}}
	view := m.View()
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
		t.Fatal("untrusted terminal escapes survived")
	}
	for _, line := range strings.Split(view, "\n") {
		if n := ansi.StringWidth(line); n > m.width {
			t.Fatalf("line is %d columns, wanted <=%d: %q", n, m.width, line)
		}
	}
	if !strings.Contains(view, "QWEN GO") {
		t.Fatal("missing app header")
	}
}

func TestStreamAndToolEventsKeepSeparateAssistantTurns(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.busy = true
	m.handleEvent(core.Event{Kind: "delta", Text: "First "})
	m.handleEvent(core.Event{Kind: "delta", Text: "answer"})
	m.handleEvent(core.Event{Kind: "tool", Name: "read_file", Text: "content"})
	m.handleEvent(core.Event{Kind: "delta", Text: "After tool"})
	m.handleEvent(core.Event{Kind: "done"})
	if len(m.entries) != 3 || m.entries[0].text != "First answer" || m.entries[2].text != "After tool" {
		t.Fatalf("bad transcript: %#v", m.entries)
	}
	if m.busy {
		t.Fatal("completion did not restore idle state")
	}
}

func TestApprovalRequiresExplicitYesAndEnterDeclines(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'y'}}} {
		m := newModel(context.Background(), Config{}, nil)
		reply := make(chan bool, 1)
		m.handleEvent(core.Event{Kind: "approval", Name: "shell", Text: "echo hello", Reply: reply})
		m.Update(key)
		want := key.Type == tea.KeyRunes
		select {
		case got := <-reply:
			if got != want {
				t.Fatalf("got approval %v wanted %v", got, want)
			}
		default:
			t.Fatal("approval response not delivered")
		}
		if m.pending != nil {
			t.Fatal("approval did not close")
		}
	}
}

func TestResetChangesSessionAndClearsChat(t *testing.T) {
	m := newModel(context.Background(), Config{SessionID: "before"}, nil)
	m.entries = []entry{{"user", "old"}}
	m.handleEvent(core.Event{Kind: "reset", Text: "after"})
	if m.cfg.SessionID != "after" || len(m.entries) != 0 {
		t.Fatalf("reset failed: %#v", m)
	}
}

func TestInputEditingSupportsUnicodeAndMultiline(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello界")})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	if got := string(m.input); got != "hello\nworld" {
		t.Fatalf("got %q", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
	if got := string(m.input); got != "hello\n!world" {
		t.Fatalf("home editing got %q", got)
	}
}

func TestCtrlCCancelsBusyTurnAndQuitsWhenIdle(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.busy = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if ctx.Err() == nil || cmd != nil {
		t.Fatal("busy Ctrl+C should cancel turn without quitting")
	}
	m.busy = false
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("idle Ctrl+C should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected quit command")
	}
}

func TestSpaceKeyIsPreserved(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	if got := string(m.input); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestApprovalRetainsFullDescriptionAndCanScroll(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	reply := make(chan bool, 1)
	text := strings.Repeat("inspect this command\n", 40) + "END_OF_COMMAND"
	m.handleEvent(core.Event{Kind: "approval", Name: "shell", Text: text, Reply: reply})
	if len(m.entries) != 1 || m.entries[0].text != text {
		t.Fatal("approval text lost")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.scroll == 0 {
		t.Fatal("approval cannot scroll")
	}
	select {
	case <-reply:
		t.Fatal("scroll inadvertently approved")
	default:
	}
}

func TestInformationalCommandsRemainVisibleAfterCompletion(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.handleEvent(core.Event{Kind: "status", Text: "Session: abc123"})
	m.handleEvent(core.Event{Kind: "done"})
	if !strings.Contains(m.View(), "Session: abc123") {
		t.Fatal("command response disappeared on completion")
	}
}

func TestApprovalRenderFitsSmallTerminal(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.width = 32
	m.height = 24
	m.handleEvent(core.Event{Kind: "approval", Name: "shell", Text: strings.Repeat("large command ", 30), Reply: make(chan bool, 1)})
	for _, line := range strings.Split(m.View(), "\n") {
		if n := ansi.StringWidth(line); n > m.width {
			t.Fatalf("approval overflows: %d columns: %q", n, line)
		}
	}
}

func TestSpinnerStopsSchedulingWhileIdle(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	_, cmd := m.Update(tickMsg{})
	if cmd != nil {
		t.Fatal("idle TUI should not schedule periodic redraws")
	}
	m.busy = true
	_, cmd = m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("busy TUI needs spinner updates")
	}
}

func receiveAppEvent(t *testing.T, m *model) eventMsg {
	t.Helper()
	result := make(chan tea.Msg, 1)
	go func() { result <- m.waitEvent()() }()
	select {
	case msg := <-result:
		e, ok := msg.(eventMsg)
		if !ok {
			t.Fatalf("expected eventMsg, got %T", msg)
		}
		return e
	case <-time.After(time.Second):
		t.Fatal("global event was not received")
		return eventMsg{}
	}
}

func TestAppGlobalAgentLifecycleVisibleAfterParentDone(t *testing.T) {
	events := make(chan core.Event, 1)
	m := newModel(context.Background(), Config{Events: events}, nil)
	m.turnID = 7
	m.busy = true
	m.Update(eventMsg{id: 7, event: core.Event{Kind: "done"}})
	events <- core.Event{Kind: "agent", Text: "Agent search finished: found three source files"}
	e := receiveAppEvent(t, m)
	if e.id != -1 {
		t.Fatalf("app event bound to parent turn %d", e.id)
	}
	m.Update(e)
	if m.busy || !strings.Contains(m.View(), "Agent search finished") {
		t.Fatal("late child completion was lost or restarted parent turn")
	}
}

func TestGlobalApprovalWorksWhileParentIdle(t *testing.T) {
	for _, answer := range []struct {
		key  rune
		want bool
	}{{'y', true}, {'n', false}} {
		t.Run(string(answer.key), func(t *testing.T) {
			events := make(chan core.Event, 1)
			reply := make(chan bool, 1)
			m := newModel(context.Background(), Config{Events: events}, nil)
			events <- core.Event{Kind: "approval", Name: "child write", Text: "Child proposes replacing result.txt", Reply: reply}
			m.Update(receiveAppEvent(t, m))
			if m.pending == nil || m.busy {
				t.Fatal("idle app did not show child approval independently")
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{answer.key}})
			select {
			case got := <-reply:
				if got != answer.want {
					t.Fatalf("approval=%v want%v", got, answer.want)
				}
			default:
				t.Fatal("child approval response missing")
			}
			if m.pending != nil {
				t.Fatal("child approval did not close")
			}
		})
	}
}

func TestGlobalApprovalSurvivesUnrelatedParentCompletion(t *testing.T) {
	events := make(chan core.Event, 1)
	reply := make(chan bool, 1)
	m := newModel(context.Background(), Config{Events: events}, nil)
	m.turnID = 3
	m.busy = true
	events <- core.Event{Kind: "approval", Name: "background child", Text: "Write child.txt?", Reply: reply}
	m.Update(receiveAppEvent(t, m))
	m.Update(eventMsg{id: 3, event: core.Event{Kind: "done"}})
	if m.pending == nil {
		t.Fatal("parent completion dismissed unrelated child approval")
	}
	select {
	case got := <-reply:
		t.Fatalf("parent completion replied to child approval: %v", got)
	default:
	}
	if !strings.Contains(m.status, "Approval") {
		t.Fatalf("pending child approval hidden by status %q", m.status)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := <-reply; !got {
		t.Fatal("explicit child approval not delivered")
	}
}

func TestStaleParentEventsRemainFilteredButGlobalEventsContinue(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.turnID = 9
	m.Update(eventMsg{id: 8, event: core.Event{Kind: "info", Text: "stale parent"}})
	m.Update(eventMsg{id: -1, event: core.Event{Kind: "agent", Text: "global child"}})
	text := m.View()
	if strings.Contains(text, "stale parent") || !strings.Contains(text, "global child") {
		t.Fatal("turn/global event filtering failed")
	}
}

func TestClosedAppEventChannelDoesNotProduceInfiniteZeroEvents(t *testing.T) {
	events := make(chan core.Event)
	close(events)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := newModel(ctx, Config{Events: events}, nil)
	m.events <- eventMsg{id: 0, event: core.Event{Kind: "info", Text: "local event still delivered"}}
	for i := 0; i < 5; i++ {
		msg := m.waitEvent()()
		if e, ok := msg.(eventMsg); ok && e.event.Text == "local event still delivered" {
			// After delivering the queued event, a closed global channel must not keep
			// the loop awake: cancel and require a quit, not another empty event.
			cancel()
			if _, ok := m.waitEvent()().(tea.QuitMsg); !ok {
				t.Fatal("closed global channel produced a spurious event")
			}
			return
		}
	}
	t.Fatal("closed app channel starved queued local event")
}

func TestInfoPluginListingRemainsFullyScrollableAndFitsViewport(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	m.width = 60
	m.height = 24
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("Plugin %02d: useful-native-tool-%02d", i, i))
	}
	text := strings.Join(lines, "\n")
	m.Update(eventMsg{id: -1, event: core.Event{Kind: "info", Text: text}})
	if len(m.entries) != 1 || m.entries[0].text != text {
		t.Fatal("plugin listing was truncated before rendering")
	}
	if !strings.Contains(m.View(), "useful-native-tool-59") {
		t.Fatal("latest plugin not visible")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlHome})
	view := m.View()
	if !strings.Contains(view, "useful-native-tool-00") {
		t.Fatal("cannot scroll to start of plugin list")
	}
	if count := len(strings.Split(view, "\n")); count > m.height {
		t.Fatalf("multiline status overflowed viewport: %d rows >%d", count, m.height)
	}
}

func TestCancelledBackgroundApprovalDoesNotDismissAnotherRequest(t *testing.T) {
	m := newModel(context.Background(), Config{}, nil)
	first, other := make(chan bool, 1), make(chan bool, 1)
	m.Update(eventMsg{-1, core.Event{Kind: "approval", Text: "background write", Reply: first}})
	m.Update(eventMsg{-1, core.Event{Kind: "approval_cancel", Reply: other}})
	if m.pending == nil {
		t.Fatal("unrelated cancellation dismissed live approval")
	}
	m.Update(eventMsg{-1, core.Event{Kind: "approval_cancel", Reply: first}})
	if m.pending != nil || m.pendingGlobal {
		t.Fatal("cancelled child left a stale approval dialog")
	}
	if approved := <-first; approved {
		t.Fatal("cancelled action approved")
	}
}
