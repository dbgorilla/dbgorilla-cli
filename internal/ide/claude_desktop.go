package ide

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// ClaudeDesktop is the Anthropic desktop app. It supports remote HTTP MCP
// only via Settings -> Connectors (UI flow, OAuth-based). The
// claude_desktop_config.json file ignores HTTP entries silently. Implement
// only Hinter, never Writer.
type ClaudeDesktop struct{}

func (c *ClaudeDesktop) Name() string { return "Claude Desktop" }
func (c *ClaudeDesktop) Slug() string { return "claude-desktop" }

func (c *ClaudeDesktop) Detect() bool {
	for _, p := range claudeDesktopAppPaths() {
		if exists(p) {
			return true
		}
	}
	return false
}

// Hint returns the manual setup instructions. Claude Desktop's HTTP MCP
// requires Settings -> Connectors and a paid plan; there's no config file
// path that accepts a Bearer token.
func (c *ClaudeDesktop) Hint(mcpURL string) string {
	return fmt.Sprintf(
		"Claude Desktop detected. Remote HTTP MCP can't be wired up via a config\n"+
			"file -- it requires the in-app connector flow:\n\n"+
			"  1. Open Claude Desktop -> Settings -> Connectors\n"+
			"  2. Add custom connector\n"+
			"  3. Paste URL: %s\n"+
			"  4. Complete the OAuth flow when prompted\n\n"+
			"Note: requires a Pro, Max, Team, or Enterprise plan. The Bearer-token\n"+
			"flow used by `dbg setup-ide` for other clients does not apply here --\n"+
			"Claude Desktop authenticates via OAuth.",
		mcpURL,
	)
}

func claudeDesktopAppPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Claude.app",
			filepath.Join(homeDir(), "Applications", "Claude.app"),
		}
	case "windows":
		return []string{
			filepath.Join(homeDir(), "AppData", "Local", "AnthropicClaude"),
		}
	default:
		return []string{
			filepath.Join(homeDir(), ".config", "Claude"),
		}
	}
}
