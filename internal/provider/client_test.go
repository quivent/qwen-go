package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qwen-go/internal/core"
)

func TestStreamAggregatesTextAndFragmentedToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var body struct {
			Model    string                `json:"model"`
			Stream   bool                  `json:"stream"`
			Messages []core.Message        `json:"messages"`
			Tools    []core.ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "test" || !body.Stream || len(body.Messages) != 1 || len(body.Tools) != 1 {
			t.Errorf("unexpected request %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keepalive\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"pa\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\"a.go\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "test"})
	var chunks []string
	message, err := client.Stream(context.Background(), []core.Message{{Role: "user", Content: "hello"}}, []core.ToolDefinition{{Type: "function", Function: core.FunctionDefinition{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}, func(s string) { chunks = append(chunks, s) })
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "Hello" || strings.Join(chunks, "") != "Hello" {
		t.Fatalf("content %+v", message)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != `{"path":"a.go"}` || message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls %+v", message.ToolCalls)
	}
}

func TestStreamRejectsTruncatedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()
	msg, err := New(Config{BaseURL: server.URL}).Stream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || msg.Content != "partial" {
		t.Fatalf("message %+v, error %v", msg, err)
	}
}
func TestStreamHTTPErrorRedactsKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "bad key super-secret")
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "super-secret"}).Stream(context.Background(), nil, nil, nil)
	if err == nil || strings.Contains(err.Error(), "super-secret") || !strings.Contains(err.Error(), "401") {
		t.Fatal(err)
	}
}
func TestStreamJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"response"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	msg, err := New(Config{BaseURL: server.URL}).Stream(context.Background(), nil, nil, nil)
	if err != nil || msg.Content != "response" {
		t.Fatalf("%+v %v", msg, err)
	}
}
func TestStreamProviderErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"quota\"}}\n\n")
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL}).Stream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatal(err)
	}
}
func TestConfigPrecedenceAndNoCredentialSerialization(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QWEN_API_KEY", "qwen-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("QWEN_MODEL", "env-model")
	t.Setenv("QWEN_BASE_URL", "https://example.test/v1")
	c, err := LoadConfig("flag-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "flag-model" || c.APIKey != "qwen-secret" || c.AuthSource != "environment" {
		t.Fatalf("incorrect config precedence")
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "secret") {
		t.Fatal("credential serialized")
	}
}
func TestConfigLocalEndpointNeedsNoKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QWEN_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	c, err := LoadConfig("local", "http://127.0.0.1:1234/v1")
	if err != nil || c.APIKey != "" {
		t.Fatalf("%v", err)
	}
}
func TestConfigReusesQwenOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QWEN_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("QWEN_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("QWEN_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	path := filepath.Join(home, ".qwen", "oauth_creds.json")
	if err := saveCredentials(path, credentials{AccessToken: "cached-token", ResourceURL: "example.test", ExpiryDate: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://example.test/v1" || c.Model != "coder-model" || c.AuthSource != "qwen-oauth" {
		t.Fatalf("config mismatch %+v", c)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credentials need 0600 mode")
	}
}
