package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

type Session struct {
	ID       string    `json:"id"`
	ParentID string    `json:"parent_id,omitempty"`
	Title    string    `json:"title"`
	CWD      string    `json:"cwd"`
	Model    string    `json:"model"`
	Updated  time.Time `json:"updated"`
	Messages []Message `json:"messages"`
}

func NewSession(cwd, model string) *Session {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &Session{ID: time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b), CWD: cwd, Model: model, Updated: time.Now().UTC()}
}

func SessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "qwen-go", "sessions"), nil
}

func SaveSession(dir string, s *Session) error {
	if !validID(s.ID) {
		return fmt.Errorf("invalid session ID")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, s.ID+".json"))
}

var sessionID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validID(id string) bool { return id != "" && sessionID.MatchString(id) }

func LoadSession(dir, id string) (*Session, error) {
	if id == "latest" {
		all, err := ListSessions(dir)
		if err != nil {
			return nil, err
		}
		if len(all) == 0 {
			return nil, fmt.Errorf("no saved sessions")
		}
		id = ""
		for _, s := range all {
			if s.ParentID == "" {
				id = s.ID
				break
			}
		}
		if id == "" {
			return nil, fmt.Errorf("no saved parent conversations")
		}
	}
	if !validID(id) {
		return nil, fmt.Errorf("invalid session ID")
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var s Session
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.ID != id {
		return nil, fmt.Errorf("session ID does not match its filename")
	}
	return &s, nil
}

func ListSessions(dir string) ([]Session, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		s, e := LoadSession(dir, entry.Name()[:len(entry.Name())-5])
		if e == nil {
			sessions = append(sessions, *s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	return sessions, nil
}
