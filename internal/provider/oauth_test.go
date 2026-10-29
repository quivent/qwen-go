package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthRefreshPreservesResourceAndRefreshToken(t *testing.T) {
	oldEndpoint := tokenEndpoint
	defer func() { tokenEndpoint = oldEndpoint }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != oauthClientID {
			t.Error("incorrect refresh request")
		}
		fmt.Fprint(w, `{"access_token":"new-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer server.Close()
	tokenEndpoint = server.URL
	path := filepath.Join(t.TempDir(), "oauth.json")
	if err := saveCredentials(path, credentials{AccessToken: "expired", RefreshToken: "old-refresh", ResourceURL: "example.test", ExpiryDate: 1}); err != nil {
		t.Fatal(err)
	}
	c, err := validCredentials(context.Background(), server.Client(), path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "new-access" || c.RefreshToken != "old-refresh" || c.ResourceURL != "example.test" || c.ExpiryDate < time.Now().UnixMilli() {
		t.Error("refresh lost credential fields")
	}
	reloaded, err := readCredentials(path)
	if err != nil || reloaded.AccessToken != "new-access" {
		t.Error("refreshed credentials not saved")
	}
}
func TestDeviceLoginPKCEAndCredentialStorage(t *testing.T) {
	oldDevice, oldToken := deviceEndpoint, tokenEndpoint
	defer func() { deviceEndpoint = oldDevice; tokenEndpoint = oldToken }()
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/device":
			if r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("code_challenge") == "" {
				t.Error("PKCE missing")
			}
			fmt.Fprint(w, `{"device_code":"private-device-code","user_code":"PUBLIC-CODE","verification_uri":"https://example.test/verify","interval":1,"expires_in":30}`)
		case "/token":
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("code_verifier") == "" || r.Form.Get("device_code") != "private-device-code" {
				t.Error("incorrect device exchange")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 3600, "resource_url": "example.test"})
		default:
			t.Error("unexpected auth endpoint")
		}
	}))
	defer server.Close()
	deviceEndpoint = server.URL + "/device"
	tokenEndpoint = server.URL + "/token"
	var out bytes.Buffer
	if err := Login(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PUBLIC-CODE") || strings.Contains(out.String(), "private-") {
		t.Error("login output must show only public verification information")
	}
	c, err := readCredentials(filepath.Join(home, ".config", "qwen-go", "oauth.json"))
	if err != nil || c.AccessToken != "private-access" {
		t.Error("login credentials not persisted")
	}
}
func TestOAuthRefreshCancellationWhileLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	if err := saveCredentials(path, credentials{RefreshToken: "refresh", ExpiryDate: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "oauth_creds.lock"), []byte("other-process"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := validCredentials(ctx, http.DefaultClient, path)
	if err == nil {
		t.Fatal("canceled refresh should fail")
	}
}

func TestOAuthExplicitEndpointSurvivesCachedResourceURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QWEN_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("QWEN_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cached" {
			t.Error("missing OAuth bearer")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"correct endpoint\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	path := filepath.Join(home, ".config", "qwen-go", "oauth.json")
	if err := saveCredentials(path, credentials{AccessToken: "cached", ResourceURL: "wrong.invalid", ExpiryDate: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig("", server.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := New(config).Stream(context.Background(), nil, nil, nil)
	if err != nil || msg.Content != "correct endpoint" {
		t.Fatalf("explicit baseURL lost: %v", err)
	}
}

func TestConcurrentRefreshOccursOnlyOnce(t *testing.T) {
	oldEndpoint := tokenEndpoint
	defer func() { tokenEndpoint = oldEndpoint }()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		time.Sleep(40 * time.Millisecond)
		fmt.Fprint(w, `{"access_token":"renewed","expires_in":3600}`)
	}))
	defer server.Close()
	tokenEndpoint = server.URL
	path := filepath.Join(t.TempDir(), "oauth.json")
	if err := saveCredentials(path, credentials{RefreshToken: "refresh", ExpiryDate: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := validCredentials(context.Background(), server.Client(), path)
			if err != nil || c.AccessToken != "renewed" {
				t.Errorf("concurrent refresh: %v", err)
			}
		}()
	}
	wg.Wait()
	if requestCount.Load() != 1 {
		t.Errorf("performed %d refreshes", requestCount.Load())
	}
}
