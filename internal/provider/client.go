// Package provider implements the streaming Qwen/OpenAI-compatible wire protocol.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"qwen-go/internal/core"
)

const DefaultBaseURL = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
const DefaultModel = "qwen3-coder-plus"

type Config struct {
	BaseURL               string `json:"base_url"`
	APIKey                string `json:"-"`
	Model                 string `json:"model"`
	AuthSource            string `json:"auth_source"`
	oauthPath             string
	oauthEndpointOverride bool
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// LoadConfig reads flags, environment and existing Qwen credentials without changing them.
// API keys are never written into session files or exposed by JSON marshaling.
func LoadConfig(model, baseURL string) (Config, error) {
	c := Config{Model: first(model, os.Getenv("QWEN_MODEL"), os.Getenv("OPENAI_MODEL")), BaseURL: first(baseURL, os.Getenv("QWEN_BASE_URL"), os.Getenv("OPENAI_BASE_URL"))}
	c.oauthEndpointOverride = c.BaseURL != ""
	c.APIKey = first(os.Getenv("QWEN_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if c.APIKey != "" {
		c.AuthSource = "environment"
	} else {
		home, _ := os.UserHomeDir()
		for _, p := range []string{filepath.Join(home, ".config", "qwen-go", "oauth.json"), filepath.Join(home, ".qwen", "oauth_creds.json")} {
			creds, err := readCredentials(p)
			if err != nil || creds.AccessToken == "" {
				continue
			}
			c.APIKey = creds.AccessToken
			c.oauthPath = p
			c.AuthSource = "qwen-oauth"
			if c.BaseURL == "" {
				c.BaseURL = oauthBaseURL(creds.ResourceURL)
			}
			break
		}
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Model == "" {
		if c.AuthSource == "qwen-oauth" {
			c.Model = "coder-model"
		} else {
			c.Model = DefaultModel
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return c, fmt.Errorf("invalid provider base URL: expected an http(s) endpoint")
	}
	// Explicit localhost endpoints may not require an API key (e.g. local inference servers).
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if c.APIKey == "" && !local {
		return c, errors.New("no credentials: set QWEN_API_KEY or OPENAI_API_KEY, or run qwen-go login")
	}
	return c, nil
}

type Client struct {
	config Config
	HTTP   *http.Client
}

func New(c Config) *Client       { return &Client{config: c, HTTP: &http.Client{}} }
func (c *Client) Config() Config { return c.config }

type wireDelta struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Index    int               `json:"index"`
		ID       string            `json:"id"`
		Type     string            `json:"type"`
		Function core.FunctionCall `json:"function"`
	} `json:"tool_calls"`
}
type chunk struct {
	Choices []struct {
		Index        int          `json:"index"`
		Delta        wireDelta    `json:"delta"`
		Message      core.Message `json:"message"`
		FinishReason *string      `json:"finish_reason"`
	} `json:"choices"`
	Error json.RawMessage `json:"error"`
}

// Stream returns the complete assistant message while delivering text as it arrives.
func (c *Client) Stream(ctx context.Context, messages []core.Message, tools []core.ToolDefinition, onDelta func(string)) (core.Message, error) {
	result := core.Message{Role: "assistant"}
	cfg := c.config
	if cfg.oauthPath != "" {
		creds, err := validCredentials(ctx, c.HTTP, cfg.oauthPath)
		if err != nil {
			return result, err
		}
		cfg.APIKey = creds.AccessToken
		if creds.ResourceURL != "" && !cfg.oauthEndpointOverride {
			cfg.BaseURL = oauthBaseURL(creds.ResourceURL)
		}
	}
	body := map[string]any{"model": cfg.Model, "messages": messages, "stream": true}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return result, err
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "qwen-go/0.1.0")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("provider request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return result, fmt.Errorf("provider HTTP %d: %s", response.StatusCode, redact(string(b), cfg.APIKey))
	}
	calls := map[int]*core.ToolCall{}
	finished := false
	consume := func(data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			finished = true
			return nil
		}
		var event chunk
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("invalid provider stream event: %w", err)
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			return fmt.Errorf("provider stream error: %s", redact(string(event.Error), cfg.APIKey))
		}
		for _, choice := range event.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != nil {
				finished = true
			}
			delta := choice.Delta
			if choice.Message.Role != "" {
				delta.Content = choice.Message.Content
				result.ToolCalls = choice.Message.ToolCalls
				finished = true
			}
			if delta.Content != "" {
				result.Content += delta.Content
				if onDelta != nil {
					onDelta(delta.Content)
				}
			}
			for _, part := range delta.ToolCalls {
				call := calls[part.Index]
				if call == nil {
					call = &core.ToolCall{Type: "function"}
					calls[part.Index] = call
				}
				call.ID += part.ID
				if part.Type != "" {
					call.Type = part.Type
				}
				call.Function.Name += part.Function.Name
				call.Function.Arguments += part.Function.Arguments
			}
		}
		return nil
	}
	if strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		b, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		if err != nil {
			return result, err
		}
		if err := consume(string(b)); err != nil {
			return result, err
		}
	} else {
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		var data []string
		dispatch := func() error {
			if len(data) == 0 {
				return nil
			}
			s := strings.Join(data, "\n")
			data = nil
			return consume(s)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if err := dispatch(); err != nil {
					return result, err
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				v := strings.TrimPrefix(line, "data:")
				v = strings.TrimPrefix(v, " ")
				data = append(data, v)
			}
		}
		if err := scanner.Err(); err != nil {
			return result, fmt.Errorf("reading provider stream: %w", err)
		}
		if err := dispatch(); err != nil {
			return result, err
		}
	}
	indices := make([]int, 0, len(calls))
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		result.ToolCalls = append(result.ToolCalls, *calls[i])
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if !finished {
		return result, errors.New("provider stream ended before a completion marker; response may be incomplete")
	}
	return result, nil
}
func redact(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return strings.TrimSpace(s)
}

// Timeout for login and refresh only. Long inference streams follow the caller's context.
var authHTTP = &http.Client{Timeout: 30 * time.Second}
