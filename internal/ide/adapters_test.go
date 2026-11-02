package ide

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the load-bearing per-adapter quirks documented in our
// research: VS Code uses "servers" not "mcpServers", Gemini uses
// "httpUrl" not "url", opencode uses type "remote", Cursor's URL-only
// shape, etc. If any of these silently flip, integration with the real
// IDE breaks and we want a unit-test failure instead of a runtime one.

func TestClaudeCode_BuildEntry_UsesHTTPType(t *testing.T) {
	got := (&ClaudeCode{}).BuildEntry("https://x/mcp/", "k")
	if got["type"] != "http" {
		t.Errorf("type = %v, want http", got["type"])
	}
	if got["url"] != "https://x/mcp/" {
		t.Errorf("url = %v", got["url"])
	}
	hdr := got["headers"].(map[string]string)
	if hdr["Authorization"] != "Bearer k" {
		t.Errorf("auth header = %q", hdr["Authorization"])
	}
}

func TestClaudeCode_ConfigPath_UserVsProject(t *testing.T) {
	c := &ClaudeCode{}
	user, _ := c.ConfigPath(ScopeUser)
	if !strings.HasSuffix(user, ".claude.json") {
		t.Errorf("user path = %q, want suffix .claude.json", user)
	}
	proj, _ := c.ConfigPath(ScopeProject)
	if filepath.Base(proj) != ".mcp.json" {
		t.Errorf("project path basename = %q, want .mcp.json", filepath.Base(proj))
	}
}

func TestCursor_ConfigPath(t *testing.T) {
	c := &Cursor{}
	user, _ := c.ConfigPath(ScopeUser)
	if !strings.HasSuffix(user, filepath.Join(".cursor", "mcp.json")) {
		t.Errorf("user path = %q", user)
	}
	proj, _ := c.ConfigPath(ScopeProject)
	if !strings.HasSuffix(proj, filepath.Join(".cursor", "mcp.json")) {
		t.Errorf("project path = %q", proj)
	}
}

func TestCursor_BuildEntry_URLOnly(t *testing.T) {
	got := (&Cursor{}).BuildEntry("https://x/mcp/", "k")
	if _, hasType := got["type"]; hasType {
		t.Errorf("Cursor entry should be URL-only (no type field), got: %+v", got)
	}
	if got["url"] != "https://x/mcp/" {
		t.Errorf("url = %v", got["url"])
	}
}

func TestVSCode_TopLevelKey_IsServers(t *testing.T) {
	if k := (&VSCode{}).TopLevelKey(); k != "servers" {
		t.Errorf("VS Code top-level key = %q, want servers (NOT mcpServers)", k)
	}
}

func TestVSCode_BuildEntry_HasHTTPType(t *testing.T) {
	got := (&VSCode{}).BuildEntry("https://x/mcp/", "k")
	if got["type"] != "http" {
		t.Errorf("type = %v, want http", got["type"])
	}
}

func TestVSCode_ConfigPath_ProjectInVscodeDir(t *testing.T) {
	proj, _ := (&VSCode{}).ConfigPath(ScopeProject)
	if !strings.HasSuffix(proj, filepath.Join(".vscode", "mcp.json")) {
		t.Errorf("project path = %q, want suffix .vscode/mcp.json", proj)
	}
}

func TestVSCode_ConfigPath_UserPerOS(t *testing.T) {
	user, _ := (&VSCode{}).ConfigPath(ScopeUser)
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(user, filepath.Join("Library", "Application Support", "Code", "User")) {
			t.Errorf("darwin user path = %q", user)
		}
	case "windows":
		if !strings.Contains(user, filepath.Join("AppData", "Roaming", "Code", "User")) {
			t.Errorf("windows user path = %q", user)
		}
	default:
		if !strings.Contains(user, filepath.Join(".config", "Code", "User")) {
			t.Errorf("linux user path = %q", user)
		}
	}
}

func TestOpencode_TopLevelKey_IsMC(t *testing.T) {
	if k := (&Opencode{}).TopLevelKey(); k != "mcp" {
		t.Errorf("opencode top-level key = %q, want mcp", k)
	}
}

func TestOpencode_BuildEntry_RemoteType(t *testing.T) {
	got := (&Opencode{}).BuildEntry("https://x/mcp/", "k")
	if got["type"] != "remote" {
		t.Errorf("type = %v, want remote", got["type"])
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true", got["enabled"])
	}
}

func TestGemini_BuildEntry_UsesHttpUrl(t *testing.T) {
	got := (&Gemini{}).BuildEntry("https://x/mcp/", "k")
	if got["httpUrl"] != "https://x/mcp/" {
		t.Errorf("httpUrl = %v (must be 'httpUrl' not 'url' for Streamable HTTP)", got["httpUrl"])
	}
	if _, hasURL := got["url"]; hasURL {
		t.Error("Gemini entry must NOT set 'url' (selects deprecated SSE transport)")
	}
}

func TestClaudeDesktop_HinterOnly(t *testing.T) {
	cd := &ClaudeDesktop{}
	if _, ok := any(cd).(Hinter); !ok {
		t.Fatal("Claude Desktop must implement Hinter")
	}
	if _, ok := any(cd).(Writer); ok {
		t.Fatal("Claude Desktop must NOT implement Writer (HTTP MCP requires UI flow)")
	}
}

func TestClaudeDesktop_HintMentionsConnectors(t *testing.T) {
	hint := (&ClaudeDesktop{}).Hint("https://x/mcp/")
	if !strings.Contains(hint, "Connectors") {
		t.Errorf("hint should reference Settings -> Connectors UI flow: %q", hint)
	}
	if !strings.Contains(hint, "https://x/mcp/") {
		t.Errorf("hint should include the MCP URL the user pastes")
	}
}

func TestAllWriters_TopLevelKeyNonEmpty(t *testing.T) {
	for _, a := range Registry {
		if w, ok := a.(Writer); ok {
			if w.TopLevelKey() == "" {
				t.Errorf("%s writer returned empty TopLevelKey", w.Slug())
			}
			if len(w.SupportedScopes()) == 0 {
				t.Errorf("%s writer returned no SupportedScopes", w.Slug())
			}
		}
	}
}

func TestJetBrains_ConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	j := &JetBrains{}
	user, _ := j.ConfigPath(ScopeUser)
	if want := filepath.Join(home, ".ai", "mcp", "mcp.json"); user != want {
		t.Errorf("user path = %q, want %q (one file shared by every JetBrains IDE)", user, want)
	}
	proj, _ := j.ConfigPath(ScopeProject)
	if !strings.HasSuffix(proj, filepath.Join(".ai", "mcp", "mcp.json")) || strings.HasPrefix(proj, home) {
		t.Errorf("project path = %q, want .ai/mcp/mcp.json under the working directory", proj)
	}
}

func TestJetBrains_BuildEntry_StreamableHTTP(t *testing.T) {
	got := (&JetBrains{}).BuildEntry("https://x/mcp/", "k")
	if got["type"] != "streamable-http" {
		t.Errorf("type = %v, want streamable-http (the type AI Assistant documents)", got["type"])
	}
	if got["url"] != "https://x/mcp/" {
		t.Errorf("url = %v", got["url"])
	}
	if hdr := got["headers"].(map[string]string); hdr["Authorization"] != "Bearer k" {
		t.Errorf("auth header = %q", hdr["Authorization"])
	}
	if k := (&JetBrains{}).TopLevelKey(); k != "mcpServers" {
		t.Errorf("top-level key = %q, want mcpServers", k)
	}
}

// TestJetBrains_Detect pins detection to the product configuration
// directories, which exist for every product and version that has run on
// the machine, on every platform.
func TestJetBrains_Detect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	prev := goos
	t.Cleanup(func() { goos = prev })

	cases := map[string][]string{
		"darwin":  {"Library", "Application Support", "JetBrains"},
		"linux":   {".config", "JetBrains"},
		"windows": {"AppData", "Roaming", "JetBrains"},
	}
	for os_, rel := range cases {
		t.Run(os_, func(t *testing.T) {
			goos = os_
			j := &JetBrains{}
			if os_ != "darwin" && j.Detect() {
				t.Fatal("detected with no configuration directory")
			}
			dir := filepath.Join(append([]string{home}, rel...)...)
			if err := osMkdirAll(filepath.Join(dir, "PhpStorm2025.2")); err != nil {
				t.Fatal(err)
			}
			if !j.Detect() {
				t.Errorf("not detected with %s present", filepath.Join(dir, "PhpStorm2025.2"))
			}
		})
	}
}

func TestJetBrains_Warnings_OnlyForIDEsOlderThan2025_3(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	prev := goos
	goos = "linux"
	t.Cleanup(func() { goos = prev })
	root := filepath.Join(home, ".config", "JetBrains")
	for _, d := range []string{"PhpStorm2025.3", "IntelliJIdea2026.1", "consentOptions"} {
		if err := osMkdirAll(filepath.Join(root, d)); err != nil {
			t.Fatal(err)
		}
	}
	j := &JetBrains{}
	if w := j.Warnings(); len(w) != 0 {
		t.Errorf("warned for current IDEs: %q", w)
	}
	for _, d := range []string{"PyCharmCE2024.3", "IdeaIC2025.2"} {
		if err := osMkdirAll(filepath.Join(root, d)); err != nil {
			t.Fatal(err)
		}
	}
	w := j.Warnings()
	if len(w) != 1 {
		t.Fatalf("want one warning naming the old IDEs, got %q", w)
	}
	for _, name := range []string{"PyCharmCE2024.3", "IdeaIC2025.2", "--print-config"} {
		if !strings.Contains(w[0], name) {
			t.Errorf("warning does not mention %s:\n%s", name, w[0])
		}
	}
	if strings.Contains(w[0], "2025.3") || strings.Contains(w[0], "2026.1") {
		t.Errorf("warning names an IDE that reads mcp.json:\n%s", w[0])
	}
}

func TestParseJetBrainsProduct(t *testing.T) {
	cases := map[string]struct {
		name string
		ok   bool
	}{
		"PhpStorm2025.2":     {"PhpStorm", true},
		"PyCharmCE2024.3":    {"PyCharmCE", true}, // longest prefix, not PyCharm + "CE2024.3"
		"IdeaIC2025.1":       {"IdeaIC", true},
		"IntelliJIdea2026.2": {"IntelliJIdea", true},
		"consentOptions":     {"", false},
		"Toolbox":            {"", false},
		"PhpStorm":           {"", false},
	}
	for dir, want := range cases {
		got, ok := parseJetBrainsProduct(dir)
		if ok != want.ok || got.name != want.name {
			t.Errorf("%s = (%q, %v), want (%q, %v)", dir, got.name, ok, want.name, want.ok)
		}
	}
}
