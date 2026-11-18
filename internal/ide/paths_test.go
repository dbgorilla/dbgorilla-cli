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

// VS Code reads the Copilot user file through Copilot CLI discovery. The path
// is the same on every OS.
func TestVSCodeConfigPath_CopilotUserFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows homeDir
	t.Setenv("COPILOT_HOME", "")

	for _, name := range []string{"darwin", "windows", "linux"} {
		t.Run(name, func(t *testing.T) {
			setGOOS(t, name)
			for _, scope := range []Scope{ScopeUser, ScopeProject} {
				got, err := (&VSCode{}).ConfigPath(scope)
				if err != nil {
					t.Fatalf("ConfigPath: %v", err)
				}
				if want := filepath.Join(home, ".copilot", "mcp-config.json"); got != want {
					t.Errorf("%s: got %q, want %q", scope, got, want)
				}
			}
		})
	}
}

func TestVSCodeConfigPath_HonoursCopilotHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	got, err := (&VSCode{}).ConfigPath(ScopeUser)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if want := filepath.Join(dir, "mcp-config.json"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The Copilot file is the portable format. "servers" there is silently ignored.
func TestVSCodeTopLevelKey_IsMcpServers(t *testing.T) {
	if got := (&VSCode{}).TopLevelKey(); got != "mcpServers" {
		t.Errorf("TopLevelKey = %q, want \"mcpServers\" for the Copilot mcp-config.json", got)
	}
}

// The files earlier versions wrote, which --remove still cleans.
func TestVSCodeLegacyPaths_PerPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

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
			legacy := (&VSCode{}).LegacyConfigs()
			if len(legacy) != 2 {
				t.Fatalf("got %d legacy configs, want 2", len(legacy))
			}
			proj, _ := legacy[0].ConfigPath(ScopeProject)
			if !strings.HasSuffix(proj, filepath.Join(".vscode", "mcp.json")) {
				t.Errorf("project legacy path %q, want .vscode/mcp.json", proj)
			}
			user, _ := legacy[1].ConfigPath(ScopeUser)
			if want := filepath.Join(append([]string{home}, tc.want...)...); user != want {
				t.Errorf("user legacy path %q, want %q", user, want)
			}
			for _, w := range legacy {
				if w.TopLevelKey() != "servers" {
					t.Errorf("legacy key %q, want servers", w.TopLevelKey())
				}
			}
		})
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
