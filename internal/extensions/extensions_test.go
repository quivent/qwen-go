package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, path, content string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func plugin(t *testing.T, base, name string) string {
	t.Helper()
	root := filepath.Join(base, name)
	write(t, root, "plugin.json", `{"name":"`+name+`","description":"Local test"}`)
	return root
}
func TestLoadNativeQwenCommandsAndSkills(t *testing.T) {
	base := t.TempDir()
	p := plugin(t, base, "developer")
	write(t, p, "commands/review.md", "---\ndescription: Review code\n---\nReview: $ARGUMENTS")
	write(t, p, "commands/git/commit.toml", "description = \"Commit message\"\nprompt = \"\"\"\nWrite a commit message for {{args}}\n\"\"\"\n")
	write(t, p, "skills/design/SKILL.md", "---\nname: design\ndescription: Design software\n---\nDesign carefully: $ARGUMENTS")
	r, err := LoadPaths(base, []string{base})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Plugins) != 1 || len(r.Commands) != 2 || len(r.Skills) != 1 {
		t.Fatalf("unexpected counts: %+v", r)
	}
	cases := map[string]string{"/review fix bug": "Review: fix bug", "/developer:review fix bug": "Review: fix bug", "/git:commit feature": "Write a commit message for feature\n", "/skill design a renderer": "Design carefully: a renderer", "/design a renderer": "Design carefully: a renderer"}
	for input, want := range cases {
		got, ok, err := r.ExpandCommand(input)
		if err != nil || !ok || got != want {
			t.Errorf("%q = %q, %t, %v; want %q", input, got, ok, err, want)
		}
	}
	if !strings.Contains(r.Instructions(), "/developer:design") || strings.Contains(r.Instructions(), "Design carefully") {
		t.Fatal("catalogue should expose names, not full skill prompts")
	}
}
func TestInlineCommandsAndLiteralArguments(t *testing.T) {
	root := t.TempDir()
	write(t, root, "plugin.json", `{"name":"inline","commands":{"hello":{"prompt":"Hello {{args}}","description":"Greeting"},"explain":"Explain carefully"}}`)
	r, err := LoadPaths(root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.ExpandCommand("/hello\t$(touch /tmp/never-execute) !{rm -rf /}")
	if err != nil || !ok || got != "Hello $(touch /tmp/never-execute) !{rm -rf /}" {
		t.Fatalf("arguments must stay literal: %q %v", got, err)
	}
	got, _, err = r.ExpandCommand("/explain sample")
	if err != nil || !strings.Contains(got, "Arguments: sample") {
		t.Fatal("missing appended arguments")
	}
}
func TestDuplicatePluginNameRejected(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	plugin(t, a, "same")
	plugin(t, b, "same")
	if _, err := LoadPaths(a, []string{a, b}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal("duplicate names must fail")
	}
}
func TestAmbiguousCommandsRequireNamespace(t *testing.T) {
	base := t.TempDir()
	a := plugin(t, base, "a")
	b := plugin(t, base, "b")
	write(t, a, "commands/review.md", "a review")
	write(t, b, "commands/review.md", "b review")
	r, err := LoadPaths(base, []string{base})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.ExpandCommand("/review"); !ok || err == nil {
		t.Fatal("ambiguous command accepted")
	}
	if out, ok, err := r.ExpandCommand("/a:review"); err != nil || !ok || out != "a review" {
		t.Fatal("qualified command failed")
	}
}
func TestPathTraversalAndSymlinksRejected(t *testing.T) {
	t.Run("traversal", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "plugin.json", `{"name":"escape","commands":{"bad":{"file":"../secret.md"}}}`)
		if _, err := LoadPaths(root, []string{root}); err == nil {
			t.Fatal("traversal accepted")
		}
	})
	t.Run("symlink file", func(t *testing.T) {
		root := plugin(t, t.TempDir(), "link")
		outside := t.TempDir()
		write(t, outside, "secret.md", "secret")
		os.Mkdir(filepath.Join(root, "commands"), 0700)
		if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "commands", "secret.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPaths(root, []string{root}); err == nil {
			t.Fatal("symlink accepted")
		}
	})
	t.Run("symlink directory", func(t *testing.T) {
		base := t.TempDir()
		outside := plugin(t, t.TempDir(), "external")
		if err := os.Symlink(outside, filepath.Join(base, "external")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPaths(base, []string{base}); err == nil {
			t.Fatal("symlink plugin directory accepted")
		}
	})
}
func TestMCPRequiresCallerTrust(t *testing.T) {
	root := t.TempDir()
	write(t, root, "qwen-extension.json", `{"name":"tools","mcpServers":{"files":{"command":"${extensionPath}/bin/server","args":["--root","${CLAUDE_PLUGIN_ROOT}"],"trusted":true}}}`)
	r, err := LoadPaths(root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	server, ok := r.MCPServers["tools:files"]
	if !ok || server.Trusted || server.Command != filepath.Join(root, "bin/server") || server.Args[1] != root {
		t.Fatal("plugin self-trusted or substitution failed")
	}
	if len(r.TrustedMCPServers(nil)) != 0 {
		t.Fatal("untrusted MCP server exposed")
	}
	trusted := r.TrustedMCPServers([]string{"tools"})
	if !trusted["tools:files"].Trusted {
		t.Fatal("explicit caller trust missing")
	}
}
func TestDisabledPluginDoesNotLoadCommands(t *testing.T) {
	root := t.TempDir()
	write(t, root, "plugin.json", `{"name":"off","enabled":false,"commands":{"ignored":"Never run"}}`)
	r, err := LoadPaths(root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Commands) != 0 || len(r.Plugins) != 1 {
		t.Fatal("disabled plugin was active")
	}
}
func TestSizeAndExpansionCaps(t *testing.T) {
	root := plugin(t, t.TempDir(), "large")
	write(t, root, "commands/huge.md", strings.Repeat("a", MaxFileBytes+1))
	if _, err := LoadPaths(root, []string{root}); err == nil {
		t.Fatal("oversized prompt accepted")
	}
	r := Registry{Commands: []Command{{Name: "expand", Plugin: "large", Prompt: "$ARGUMENTS"}}}
	if _, _, err := r.ExpandCommand("/expand " + strings.Repeat("a", MaxFileBytes+1)); err == nil {
		t.Fatal("oversized expansion accepted")
	}
}
func TestUnknownCommandAndMissingDirectories(t *testing.T) {
	base := t.TempDir()
	r, err := LoadPaths(base, []string{filepath.Join(base, "absent")})
	if err != nil {
		t.Fatal(err)
	}
	out, matched, err := r.ExpandCommand("/unknown hi")
	if err != nil || matched || out != "/unknown hi" {
		t.Fatal("unknown command should pass through")
	}
	if _, _, err := r.ExpandCommand("/skill"); err == nil {
		t.Fatal("missing skill name needs usage error")
	}
}
