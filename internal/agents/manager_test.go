package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qwen-go/internal/core"
)

type streamFunc func(context.Context, []core.Message, []core.ToolDefinition, func(string)) (core.Message, error)

func (f streamFunc) Stream(ctx context.Context, m []core.Message, d []core.ToolDefinition, emit func(string)) (core.Message, error) {
	return f(ctx, m, d, emit)
}
func tool(name string, args map[string]any) core.ToolCall {
	b, _ := json.Marshal(args)
	return core.ToolCall{Function: core.FunctionCall{Name: name, Arguments: string(b)}}
}
func invoke(t *testing.T, m *Manager, name string, args map[string]any) string {
	t.Helper()
	s, e := m.Execute(context.Background(), tool(name, args))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func spawn(t *testing.T, m *Manager, name string) agentInfo {
	t.Helper()
	s := invoke(t, m, "spawn_agent", map[string]any{"name": name, "prompt": "Do the task"})
	var info agentInfo
	if err := json.Unmarshal([]byte(s), &info); err != nil {
		t.Fatal(err)
	}
	return info
}
func wait(t *testing.T, m *Manager, id string) agentInfo {
	t.Helper()
	s := invoke(t, m, "wait_agent", map[string]any{"id": id, "timeout_seconds": 2})
	var info agentInfo
	json.Unmarshal([]byte(s), &info)
	return info
}
func opts(t *testing.T, client core.Streamer) Options {
	return Options{CWD: t.TempDir(), StoreDir: t.TempDir(), Model: "test", Client: client}
}

func TestParallelBoundedExecution(t *testing.T) {
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	var active, peak atomic.Int32
	c := streamFunc(func(ctx context.Context, _ []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return core.Message{}, ctx.Err()
		}
		return core.Message{Content: "finished"}, nil
	})
	o := opts(t, c)
	o.MaxConcurrent = 2
	m := New(context.Background(), o)
	defer m.Close()
	a := spawn(t, m, "one")
	b := spawn(t, m, "two")
	third := spawn(t, m, "three")
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("agents did not run concurrently")
		}
	}
	select {
	case <-entered:
		t.Fatal("concurrency limit exceeded")
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	for _, id := range []string{a.ID, b.ID, third.ID} {
		result := wait(t, m, id)
		if result.Status != "completed" || result.Result != "finished" {
			t.Fatalf("bad result %+v", result)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
}

func TestReadOnlyEnforcedEvenForHallucinatedMutation(t *testing.T) {
	var calls atomic.Int32
	c := streamFunc(func(_ context.Context, messages []core.Message, defs []core.ToolDefinition, _ func(string)) (core.Message, error) {
		if len(defs) != 3 {
			t.Errorf("readonly tools count %d", len(defs))
		}
		if calls.Add(1) == 1 {
			return core.Message{ToolCalls: []core.ToolCall{{ID: "bad", Type: "function", Function: core.FunctionCall{Name: "write_file", Arguments: `{"path":"bad","content":"bad"}`}}}}, nil
		}
		last := messages[len(messages)-1]
		if !strings.Contains(last.Content, "read-only") {
			t.Errorf("mutation not denied: %+v", last)
		}
		return core.Message{Content: "did not write"}, nil
	})
	o := opts(t, c)
	o.AllowAll = true
	m := New(context.Background(), o)
	defer m.Close()
	a := spawn(t, m, "reader")
	result := wait(t, m, a.ID)
	if result.Status != "completed" {
		t.Fatalf("status %+v", result)
	}
	if _, e := os.Stat(filepath.Join(o.CWD, "bad")); !os.IsNotExist(e) {
		t.Fatal("readonly agent wrote a file")
	}
}

func TestWriteSpawnAndMutationRequireSeparateApprovals(t *testing.T) {
	var calls, approvals atomic.Int32
	c := streamFunc(func(_ context.Context, _ []core.Message, defs []core.ToolDefinition, _ func(string)) (core.Message, error) {
		if len(defs) != 6 {
			t.Errorf("write tools count %d", len(defs))
		}
		if calls.Add(1) == 1 {
			return core.Message{ToolCalls: []core.ToolCall{{ID: "write", Type: "function", Function: core.FunctionCall{Name: "write_file", Arguments: `{"path":"ok","content":"approved"}`}}}}, nil
		}
		return core.Message{Content: "done"}, nil
	})
	o := opts(t, c)
	o.Approve = func(context.Context, string) bool { approvals.Add(1); return true }
	m := New(context.Background(), o)
	defer m.Close()
	s := invoke(t, m, "spawn_agent", map[string]any{"name": "writer", "prompt": "write", "mode": "write"})
	var a agentInfo
	json.Unmarshal([]byte(s), &a)
	wait(t, m, a.ID)
	if approvals.Load() != 2 {
		t.Fatalf("wanted spawn and mutation approval; got %d", approvals.Load())
	}
	data, e := os.ReadFile(filepath.Join(o.CWD, "ok"))
	if e != nil || string(data) != "approved" {
		t.Fatalf("file %q %v", data, e)
	}
}

func TestCancelAndFollowup(t *testing.T) {
	started := make(chan struct{}, 1)
	blocking := streamFunc(func(ctx context.Context, _ []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
		started <- struct{}{}
		<-ctx.Done()
		return core.Message{}, ctx.Err()
	})
	m := New(context.Background(), opts(t, blocking))
	a := spawn(t, m, "blocked")
	<-started
	if _, e := m.Execute(context.Background(), tool("send_agent_message", map[string]any{"id": a.ID, "message": "too early"})); e == nil {
		t.Fatal("message accepted while running")
	}
	invoke(t, m, "cancel_agent", map[string]any{"id": a.ID})
	if result := wait(t, m, a.ID); result.Status != "cancelled" {
		t.Fatalf("not cancelled %+v", result)
	}
	m.Close()

	countUsers := streamFunc(func(_ context.Context, messages []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
		n := 0
		for _, message := range messages {
			if message.Role == "user" {
				n++
			}
		}
		return core.Message{Content: fmt.Sprintf("users=%d", n)}, nil
	})
	o := opts(t, countUsers)
	o.ParentID = "parent-session"
	m2 := New(context.Background(), o)
	defer m2.Close()
	b := spawn(t, m2, "follow")
	if got := wait(t, m2, b.ID); got.Result != "users=1" {
		t.Fatalf("first %+v", got)
	}
	invoke(t, m2, "send_agent_message", map[string]any{"id": b.ID, "message": "continue"})
	if got := wait(t, m2, b.ID); got.Result != "users=2" {
		t.Fatalf("followup %+v", got)
	}
	saved, e := core.LoadSession(o.StoreDir, b.ID)
	if e != nil || saved.ParentID != "parent-session" || len(saved.Messages) != 4 {
		t.Fatalf("saved %+v %v", saved, e)
	}
}

func TestWaitTimeoutAndClose(t *testing.T) {
	started := make(chan struct{}, 1)
	c := streamFunc(func(ctx context.Context, _ []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
		started <- struct{}{}
		<-ctx.Done()
		return core.Message{}, ctx.Err()
	})
	m := New(context.Background(), opts(t, c))
	a := spawn(t, m, "wait")
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := m.Execute(ctx, tool("wait_agent", map[string]any{"id": a.ID})); e == nil {
		t.Fatal("wait ignored cancellation")
	}
	start := time.Now()
	m.Close()
	if time.Since(start) > time.Second {
		t.Fatal("Close did not cancel/join")
	}
	if _, e := m.Execute(context.Background(), tool("spawn_agent", map[string]any{"name": "late", "prompt": "no"})); e == nil {
		t.Fatal("spawn after close")
	}
}

func TestFollowupCancellationDoesNotReturnStaleResult(t *testing.T) {
	var rounds atomic.Int32
	started := make(chan struct{}, 1)
	c := streamFunc(func(ctx context.Context, _ []core.Message, _ []core.ToolDefinition, _ func(string)) (core.Message, error) {
		if rounds.Add(1) == 1 {
			return core.Message{Content: "old result"}, nil
		}
		started <- struct{}{}
		<-ctx.Done()
		return core.Message{}, ctx.Err()
	})
	o := opts(t, c)
	m := New(context.Background(), o)
	defer m.Close()
	a := spawn(t, m, "followup")
	wait(t, m, a.ID)
	invoke(t, m, "send_agent_message", map[string]any{"id": a.ID, "message": "new task"})
	<-started
	invoke(t, m, "cancel_agent", map[string]any{"id": a.ID})
	result := wait(t, m, a.ID)
	if result.Result != "" || result.Status != "cancelled" {
		t.Fatalf("stale result after cancelled followup %+v", result)
	}
}

func TestParentResetAppliesOnlyToFutureChildren(t *testing.T) {
	c := streamFunc(func(context.Context, []core.Message, []core.ToolDefinition, func(string)) (core.Message, error) {
		return core.Message{Content: "ok"}, nil
	})
	o := opts(t, c)
	o.ParentID = "before"
	m := New(context.Background(), o)
	defer m.Close()
	a := spawn(t, m, "before")
	wait(t, m, a.ID)
	m.SetParentID("after")
	b := spawn(t, m, "after")
	wait(t, m, b.ID)
	for id, want := range map[string]string{a.ID: "before", b.ID: "after"} {
		s, e := core.LoadSession(o.StoreDir, id)
		if e != nil || s.ParentID != want {
			t.Fatalf("parent %s %+v %v", want, s, e)
		}
	}
}
