package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OAuth constants and credential format match Qwen Code 0.24.5's qwenOAuth2.ts.
const oauthClientID = "f0304373b74a44d2b584a3fb70ca9e56"

var deviceEndpoint = "https://chat.qwen.ai/api/v1/oauth2/device/code"
var tokenEndpoint = "https://chat.qwen.ai/api/v1/oauth2/token"

type credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ResourceURL  string `json:"resource_url,omitempty"`
	ExpiryDate   int64  `json:"expiry_date"`
}

func readCredentials(path string) (credentials, error) {
	var c credentials
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(b, &c)
	return c, e
}
func oauthBaseURL(resource string) string {
	if resource == "" {
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if !strings.HasPrefix(resource, "https://") && !strings.HasPrefix(resource, "http://") {
		resource = "https://" + resource
	}
	resource = strings.TrimRight(resource, "/")
	if !strings.HasSuffix(resource, "/v1") {
		resource += "/v1"
	}
	return resource
}
func saveCredentials(path string, c credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".oauth-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type oauthResponse struct {
	AccessToken             string `json:"access_token"`
	RefreshToken            string `json:"refresh_token"`
	TokenType               string `json:"token_type"`
	ResourceURL             string `json:"resource_url"`
	ExpiresIn               int64  `json:"expires_in"`
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int64  `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

func authPost(ctx context.Context, client *http.Client, endpoint string, values url.Values) (oauthResponse, error) {
	var out oauthResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer response.Body.Close()
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("OAuth HTTP %d: invalid JSON response", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if out.Error == "" {
			return out, fmt.Errorf("OAuth HTTP %d", response.StatusCode)
		}
	}
	if out.Error != "" {
		return out, fmt.Errorf("Qwen OAuth: %s", out.Error)
	}
	return out, nil
}
func responseCredentials(r oauthResponse, previous credentials) credentials {
	c := credentials{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenType: r.TokenType, ResourceURL: r.ResourceURL, ExpiryDate: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli()}
	if c.RefreshToken == "" {
		c.RefreshToken = previous.RefreshToken
	}
	if c.ResourceURL == "" {
		c.ResourceURL = previous.ResourceURL
	}
	return c
}
func validCredentials(ctx context.Context, client *http.Client, path string) (credentials, error) {
	c, err := readCredentials(path)
	if err != nil {
		return c, fmt.Errorf("read Qwen credentials: %w", err)
	}
	if c.AccessToken != "" && (c.ExpiryDate == 0 || time.Until(time.UnixMilli(c.ExpiryDate)) > 30*time.Second) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, errors.New("Qwen credentials expired; run qwen-go login")
	}
	// Use the same exclusive lock filename as Qwen Code when reusing its cache.
	lockPath := filepath.Join(filepath.Dir(path), "oauth_creds.lock")
	deadline := time.Now().Add(35 * time.Second)
	for {
		f, e := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			fmt.Fprintf(f, "qwen-go-%d", os.Getpid())
			f.Close()
			break
		}
		if !errors.Is(e, os.ErrExist) {
			return c, e
		}
		if time.Now().After(deadline) {
			return c, errors.New("Qwen token refresh is locked by another process; retry shortly")
		}
		select {
		case <-ctx.Done():
			return c, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	defer os.Remove(lockPath)
	// Another process may have refreshed credentials while we waited.
	if newer, e := readCredentials(path); e == nil {
		c = newer
		if c.AccessToken != "" && time.Until(time.UnixMilli(c.ExpiryDate)) > 30*time.Second {
			return c, nil
		}
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := authPost(refreshCtx, client, tokenEndpoint, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}, "client_id": {oauthClientID}})
	if err != nil {
		return c, fmt.Errorf("refresh Qwen login: %w", err)
	}
	if response.AccessToken == "" {
		return c, errors.New("Qwen refresh returned no access token")
	}
	c = responseCredentials(response, c)
	if err = saveCredentials(path, c); err != nil {
		return c, err
	}
	return c, nil
}

// Login performs Qwen's device flow entirely in Go. It prints only a verification
// link and user code, never access or refresh tokens. Browser consent remains manual.
func Login(ctx context.Context, out io.Writer) error {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	response, err := authPost(ctx, authHTTP, deviceEndpoint, url.Values{"client_id": {oauthClientID}, "scope": {"openid profile email model.completion"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	if err != nil {
		return err
	}
	if response.DeviceCode == "" {
		return errors.New("Qwen login returned no device code")
	}
	link := first(response.VerificationURIComplete, response.VerificationURI)
	fmt.Fprintf(out, "Open %s\nCode: %s\nWaiting for browser approval (Ctrl+C cancels)…\n", link, response.UserCode)
	interval := time.Duration(response.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expiry := response.ExpiresIn
	if expiry <= 0 {
		expiry = 600
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(expiry)*time.Second)
	defer cancel()
	for {
		select {
		case <-pollCtx.Done():
			return pollCtx.Err()
		case <-time.After(interval):
		}
		token, pollErr := authPost(pollCtx, authHTTP, tokenEndpoint, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {oauthClientID}, "device_code": {response.DeviceCode}, "code_verifier": {verifier}})
		if token.Error == "authorization_pending" {
			continue
		}
		if token.Error == "slow_down" {
			interval += 5 * time.Second
			continue
		}
		if pollErr != nil {
			return pollErr
		}
		if token.AccessToken == "" {
			return errors.New("Qwen login returned no access token")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path := filepath.Join(home, ".config", "qwen-go", "oauth.json")
		if err = saveCredentials(path, responseCredentials(token, credentials{})); err != nil {
			return err
		}
		fmt.Fprintln(out, "Qwen login saved securely. Run qwen-go to start.")
		return nil
	}
}
