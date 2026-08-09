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
