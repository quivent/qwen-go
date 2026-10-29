// Package extensions loads local prompt plugins without running installation,
// lifecycle hooks, JavaScript, or external tool servers. Qwen's extension manifest,
// Markdown commands and SKILL.md layout are supported as a native subset.
package extensions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 64 * 1024
const MaxTotalBytes = 512 * 1024
const MaxPlugins = 64
const MaxEntries = 256

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*(?::[a-zA-Z0-9][a-zA-Z0-9_-]*)*$`)

type Plugin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Root        string `json:"root"`
	Enabled     bool   `json:"enabled"`
}
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"-"`
	Plugin      string `json:"plugin"`
	Path        string `json:"path,omitempty"`
}
type Skill struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"-"`
	Plugin       string `json:"plugin"`
	Path         string `json:"path"`
}
type MCPServer struct {
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Type      string            `json:"type,omitempty"`
	Plugin    string            `json:"plugin"`
	Root      string            `json:"root"`
	Enabled   bool              `json:"enabled"`
	Trusted   bool              `json:"trusted"`
}
type Registry struct {
	Plugins    []Plugin             `json:"plugins"`
	Commands   []Command            `json:"commands"`
	Skills     []Skill              `json:"skills"`
	MCPServers map[string]MCPServer `json:"mcp_servers"`
	bytes      int
}
type manifest struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Version     string                     `json:"version"`
	Enabled     *bool                      `json:"enabled"`
	Commands    map[string]json.RawMessage `json:"commands"`
	MCPServers  map[string]MCPServer       `json:"mcpServers"`
}

// Load searches native plugin directories and read-only legacy Qwen extensions.
// Missing directories are normal. No plugin installs, hooks or servers are run.
func Load(cwd string) (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return LoadPaths(cwd, []string{filepath.Join(home, ".config", "qwen-go", "plugins"), filepath.Join(cwd, ".qwen-go", "plugins"), filepath.Join(home, ".qwen", "extensions"), filepath.Join(cwd, ".qwen", "extensions")})
}

// LoadPaths accepts directories containing plugins, or a single manifest directory.
func LoadPaths(cwd string, paths []string) (*Registry, error) {
	r := &Registry{MCPServers: map[string]MCPServer{}}
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("plugin location must be a real directory: %s", path)
		}
		roots := []string{}
		if manifestName(path) != "" {
			roots = append(roots, path)
		} else {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Type()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("symlink plugin directory refused: %s", filepath.Join(path, e.Name()))
				}
				if e.IsDir() && manifestName(filepath.Join(path, e.Name())) != "" {
					roots = append(roots, filepath.Join(path, e.Name()))
				}
			}
		}
		for _, root := range roots {
			if seen[root] {
				continue
			}
			seen[root] = true
			if len(r.Plugins) >= MaxPlugins {
				return nil, errors.New("too many plugins")
			}
			if err := r.load(root); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(r.Plugins, func(i, j int) bool { return r.Plugins[i].Name < r.Plugins[j].Name })
	sort.Slice(r.Commands, func(i, j int) bool {
		a, b := r.Commands[i], r.Commands[j]
		return a.Plugin+":"+a.Name < b.Plugin+":"+b.Name
	})
	sort.Slice(r.Skills, func(i, j int) bool {
		a, b := r.Skills[i], r.Skills[j]
		return a.Plugin+":"+a.Name < b.Plugin+":"+b.Name
	})
	return r, nil
}
func manifestName(root string) string {
	for _, n := range []string{"plugin.json", "qwen-extension.json", "gemini-extension.json"} {
		if _, err := os.Lstat(filepath.Join(root, n)); err == nil {
			return n
		}
	}
	return ""
}
func (r *Registry) read(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("plugin paths must be relative")
	}
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("plugin path escapes its directory")
	}
	current := root
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("plugin symlink refused: %s", current)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("plugin content must be a regular file")
	}
	if info.Size() > MaxFileBytes {
		return "", fmt.Errorf("plugin file exceeds %d bytes: %s", MaxFileBytes, current)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return "", err
	}
	if len(data) > MaxFileBytes {
		return "", errors.New("plugin file grew beyond limit")
	}
	r.bytes += len(data)
	if r.bytes > MaxTotalBytes {
		return "", errors.New("plugin content exceeds total size limit")
	}
	if !utf8.Valid(data) {
		return "", errors.New("plugin content must be UTF-8")
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}
func (r *Registry) load(root string) error {
	raw, err := r.read(root, manifestName(root))
	if err != nil {
		return err
	}
	var m manifest
	if err = json.Unmarshal([]byte(raw), &m); err != nil {
		return fmt.Errorf("plugin manifest %s: %w", root, err)
	}
	if !namePattern.MatchString(m.Name) || strings.Contains(m.Name, ":") {
		return fmt.Errorf("invalid plugin name %q", m.Name)
	}
	for _, p := range r.Plugins {
		if p.Name == m.Name {
			return fmt.Errorf("duplicate plugin name %q", m.Name)
		}
	}
	enabled := m.Enabled == nil || *m.Enabled
	r.Plugins = append(r.Plugins, Plugin{Name: m.Name, Description: m.Description, Version: m.Version, Root: root, Enabled: enabled})
	if !enabled {
		return nil
	}
	for name, raw := range m.Commands {
		var prompt string
		description := ""
		source := ""
		if json.Unmarshal(raw, &prompt) != nil {
			var spec struct {
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
				File        string `json:"file"`
			}
			if err = json.Unmarshal(raw, &spec); err != nil {
				return err
			}
			prompt, description, source = spec.Prompt, spec.Description, spec.File
			if source != "" {
				prompt, err = r.read(root, source)
				if err != nil {
					return err
				}
			}
		}
		if err = r.addCommand(Command{Name: name, Description: description, Prompt: prompt, Plugin: m.Name, Path: source}); err != nil {
			return err
		}
	}
	for _, dir := range []string{"commands", "skills"} {
		path := filepath.Join(root, dir)
		if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = filepath.WalkDir(path, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("plugin symlink refused: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if dir == "skills" && d.Name() != "SKILL.md" {
				return nil
			}
			if dir == "commands" && filepath.Ext(path) != ".md" && filepath.Ext(path) != ".toml" {
				return nil
			}
			text, err := r.read(root, rel)
			if err != nil {
				return err
			}
			if dir == "skills" {
				fields, body, err := frontmatter(text)
				if err != nil {
					return err
				}
				name := fields["name"]
				if name == "" {
					name = filepath.Base(filepath.Dir(path))
				}
				if !namePattern.MatchString(name) {
					return fmt.Errorf("invalid skill name %q", name)
				}
				if len(r.Skills)+len(r.Commands) >= MaxEntries {
					return errors.New("too many plugin commands/skills")
				}
				for _, s := range r.Skills {
					if s.Plugin == m.Name && s.Name == name {
						return fmt.Errorf("duplicate skill %s:%s", m.Name, name)
					}
				}
				r.Skills = append(r.Skills, Skill{Name: name, Description: fields["description"], Instructions: body, Plugin: m.Name, Path: path})
				return nil
			}
			commandRel, _ := filepath.Rel(filepath.Join(root, "commands"), path)
			name := strings.ReplaceAll(strings.TrimSuffix(commandRel, filepath.Ext(commandRel)), string(filepath.Separator), ":")
			var description, prompt string
			if filepath.Ext(path) == ".toml" {
				fields, e := parseTOML(text)
				if e != nil {
					return e
				}
				description, prompt = fields["description"], fields["prompt"]
			} else {
				fields, body, e := frontmatter(text)
				if e != nil {
					return e
				}
				description, prompt = fields["description"], body
			}
			return r.addCommand(Command{Name: name, Description: description, Prompt: prompt, Plugin: m.Name, Path: path})
		}); err != nil {
			return fmt.Errorf("plugin %s: %w", m.Name, err)
		}
	}
	for name, server := range m.MCPServers {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid MCP server name %q", name)
		}
		if server.Command == "" && server.URL == "" {
			return fmt.Errorf("MCP server %s needs command or URL", name)
		}
		// Plugin manifests cannot grant trust to themselves. The caller must explicitly
		// approve the plugin before these definitions can be passed to an MCP client.
		server.Plugin = m.Name
		server.Root = root
		server.Enabled = true
		server.Trusted = false
		server.Command = variables(server.Command, root)
		server.URL = variables(server.URL, root)
		for i, arg := range server.Args {
			server.Args[i] = variables(arg, root)
		}
		for k, v := range server.Env {
			server.Env[k] = variables(v, root)
		}
		r.MCPServers[m.Name+":"+name] = server
	}
	return nil
}
func (r *Registry) addCommand(c Command) error {
	if !namePattern.MatchString(c.Name) || strings.TrimSpace(c.Prompt) == "" {
		return fmt.Errorf("invalid or empty plugin command %q", c.Name)
	}
	if len(c.Prompt) > MaxFileBytes {
		return errors.New("command prompt exceeds size limit")
	}
	if len(r.Commands)+len(r.Skills) >= MaxEntries {
		return errors.New("too many plugin commands/skills")
	}
	for _, existing := range r.Commands {
		if existing.Plugin == c.Plugin && existing.Name == c.Name {
			return fmt.Errorf("duplicate plugin command %s:%s", c.Plugin, c.Name)
		}
	}
	r.Commands = append(r.Commands, c)
	return nil
}
func variables(s, root string) string {
	return strings.NewReplacer("${extensionPath}", root, "${CLAUDE_PLUGIN_ROOT}", root).Replace(s)
}

// ExpandCommand expands explicit /name or /plugin:name invocations into text.
// Argument substitution is literal: no shell, template evaluation, file reads or
// command substitutions are performed. Ambiguous names require qualification.
func (r *Registry) ExpandCommand(input string) (string, bool, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return input, false, nil
	}
	token, args := splitInput(input[1:])
	if token == "skill" {
		name, arguments := splitInput(args)
		return r.ExpandSkill(name, arguments)
	}
	type match struct{ name, prompt string }
	matches := []match{}
	for _, c := range r.Commands {
		if token == c.Name || token == c.Plugin+":"+c.Name {
			matches = append(matches, match{c.Plugin + ":" + c.Name, c.Prompt})
		}
	}
	for _, s := range r.Skills {
		if token == s.Name || token == s.Plugin+":"+s.Name {
			matches = append(matches, match{s.Plugin + ":" + s.Name, s.Instructions})
		}
	}
	if len(matches) == 0 {
		return input, false, nil
	}
	if len(matches) > 1 {
		return "", true, fmt.Errorf("ambiguous plugin command /%s; use a plugin-qualified name", token)
	}
	prompt := matches[0].prompt
	placeholder := strings.Contains(prompt, "$ARGUMENTS") || strings.Contains(prompt, "{{args}}")
	prompt = strings.ReplaceAll(prompt, "$ARGUMENTS", args)
	prompt = strings.ReplaceAll(prompt, "{{args}}", args)
	if !placeholder && args != "" {
		prompt += "\n\nArguments: " + args
	}
	if len(prompt) > MaxFileBytes {
		return "", true, errors.New("expanded plugin prompt exceeds size limit")
	}
	return prompt, true, nil
}

// Instructions supplies only the skill catalogue. Full skill instructions are
// loaded into the conversation only when the user explicitly invokes that skill.
func (r *Registry) Instructions() string {
	if len(r.Commands)+len(r.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Local plugin commands and skills available on explicit user request (invoke /plugin:command or /skill plugin:name [arguments]):\n")
	for _, c := range r.Commands {
		fmt.Fprintf(&b, "- /%s:%s — %s\n", c.Plugin, c.Name, c.Description)
	}
	for _, s := range r.Skills {
		fmt.Fprintf(&b, "- /%s:%s (skill) — %s\n", s.Plugin, s.Name, s.Description)
	}
	return b.String()
}

// TrustedMCPServers returns only definitions from explicitly named plugins.
// Loading a workspace plugin never automatically starts its external servers.
func (r *Registry) TrustedMCPServers(trustedPlugins []string) map[string]MCPServer {
	trusted := map[string]bool{}
	for _, name := range trustedPlugins {
		trusted[name] = true
	}
	out := map[string]MCPServer{}
	for name, s := range r.MCPServers {
		if trusted[s.Plugin] && s.Enabled {
			s.Trusted = true
			out[name] = s
		}
	}
	return out
}
func (r *Registry) List() string {
	if len(r.Plugins) == 0 {
		return "No local plugins loaded."
	}
	var b strings.Builder
	for _, p := range r.Plugins {
		state := "enabled"
		if !p.Enabled {
			state = "disabled"
		}
		fmt.Fprintf(&b, "%s %s [%s] — %s\n", p.Name, p.Version, state, p.Description)
	}
	for _, c := range r.Commands {
		fmt.Fprintf(&b, "  /%s:%s — %s\n", c.Plugin, c.Name, c.Description)
	}
	for _, s := range r.Skills {
		fmt.Fprintf(&b, "  /%s:%s (skill) — %s\n", s.Plugin, s.Name, s.Description)
	}
	keys := make([]string, 0, len(r.MCPServers))
	for k := range r.MCPServers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  MCP %s [requires startup and tool approval]\n", k)
	}
	return strings.TrimSpace(b.String())
}

// frontmatter intentionally handles only string metadata needed by prompt
// commands and skills. Lifecycle/tool permissions in YAML are never executed.
func frontmatter(text string) (map[string]string, string, error) {
	fields := map[string]string{}
	if !strings.HasPrefix(text, "---\n") {
		return fields, text, nil
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil, "", errors.New("unclosed Markdown frontmatter")
	}
	metadata := text[4 : 4+end]
	body := text[4+end+5:]
	for _, line := range strings.Split(metadata, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		fields[strings.TrimSpace(key)] = value
	}
	return fields, body, nil
}

// parseTOML reads the legacy command subset: description/prompt as quoted single-
// line strings or triple-quoted multiline strings. Other TOML is rejected.
func parseTOML(text string) (map[string]string, error) {
	fields := map[string]string{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("unsupported command TOML; use Markdown or quoted prompt/description")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "prompt" && key != "description" {
			continue
		}
		if strings.HasPrefix(value, `"""`) || strings.HasPrefix(value, "'''") {
			quote := value[:3]
			value = value[3:]
			parts := []string{}
			closed := false
			for {
				if end := strings.Index(value, quote); end >= 0 {
					parts = append(parts, value[:end])
					closed = true
					break
				}
				parts = append(parts, value)
				i++
				if i >= len(lines) {
					break
				}
				value = lines[i]
			}
			if !closed {
				return nil, errors.New("unclosed command TOML multiline string")
			}
			fields[key] = strings.TrimPrefix(strings.Join(parts, "\n"), "\n")
		} else if strings.HasPrefix(value, `"`) {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, err
			}
			fields[key] = decoded
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			fields[key] = value[1 : len(value)-1]
		} else {
			return nil, errors.New("command TOML strings must be quoted")
		}
	}
	if fields["prompt"] == "" {
		return nil, errors.New("command TOML needs a prompt")
	}
	return fields, nil
}

func splitInput(input string) (string, string) {
	input = strings.TrimSpace(input)
	if index := strings.IndexFunc(input, unicode.IsSpace); index >= 0 {
		return input[:index], strings.TrimSpace(input[index:])
	}
	return input, ""
}

// ExpandSkill expands /skill NAME arguments without confusing a same-named command.
func (r *Registry) ExpandSkill(name, args string) (string, bool, error) {
	if name == "" {
		return "", true, errors.New("usage: /skill NAME [arguments]")
	}
	matches := []Skill{}
	for _, s := range r.Skills {
		if name == s.Name || name == s.Plugin+":"+s.Name {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return "", true, fmt.Errorf("unknown skill %q", name)
	}
	if len(matches) > 1 {
		return "", true, fmt.Errorf("ambiguous skill %q; use plugin:name", name)
	}
	s := matches[0]
	temporary := Registry{Skills: []Skill{s}}
	return temporary.ExpandCommand("/" + s.Plugin + ":" + s.Name + " " + args)
}
