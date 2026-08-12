package ide

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every adapter resolves a different config directory per platform. A wrong
// path does not error — it writes a file the IDE never reads, and the user is
// told setup succeeded. These pin all three platforms from any host.

func setGOOS(t *testing.T, v string) {
	t.Helper()
	orig := goos
	goos = v
	t.Cleanup(func() { goos = orig })
}

func TestVSCodeConfigPath_PerPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows homeDir

	cases := []struct {
		os   string
		want []string
	}{
		{"darwin", []string{"Library", "Application Support", "Code", "User", "mcp.json"}},
		{"windows", []string{"AppData", "Roaming", "Code", "User", "mcp.json"}},
		{"linux", []string{".config", "Code", "User", "mcp.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.os, func(t *testing.T) {
			setGOOS(t, tc.os)
			got, err := (&VSCode{}).ConfigPath(ScopeUser)
			if err != nil {
				t.Fatalf("ConfigPath: %v", err)
			}
			if want := filepath.Join(append([]string{homeDir()}, tc.want...)...); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// Project scope is the same everywhere: next to the workspace.
func TestVSCodeConfigPath_ProjectScope(t *testing.T) {
	setGOOS(t, "windows") // must not matter
	got, err := (&VSCode{}).ConfigPath(ScopeProject)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".vscode", "mcp.json")) {
		t.Errorf("got %q, want a workspace-relative .vscode/mcp.json", got)
	}
}

// The load-bearing difference between VS Code and everything else.
func TestVSCodeTopLevelKey_IsServersNotMcpServers(t *testing.T) {
	if got := (&VSCode{}).TopLevelKey(); got != "servers" {
		t.Errorf("TopLevelKey = %q, want \"servers\" — \"mcpServers\" silently does nothing in VS Code", got)
	}
}

func TestClaudeDesktopPaths_PerPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for _, os := range []string{"darwin", "windows", "linux"} {
		t.Run(os, func(t *testing.T) {
			setGOOS(t, os)
			paths := claudeDesktopAppPaths()
			// Linux has no Claude Desktop build; the others must offer at least
			// one place to look or detection can never succeed.
			if os != "linux" && len(paths) == 0 {
				t.Errorf("%s should have at least one candidate path", os)
			}
		})
	}
}

func TestClaudeCodeConfigPath_PerPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for _, os := range []string{"darwin", "windows", "linux"} {
		t.Run(os, func(t *testing.T) {
			setGOOS(t, os)
			got, err := (&ClaudeCode{}).ConfigPath(ScopeUser)
			if err != nil {
				t.Fatalf("ConfigPath: %v", err)
			}
			if got == "" || !filepath.IsAbs(got) {
				t.Errorf("path = %q, want an absolute location", got)
			}
		})
	}
}
