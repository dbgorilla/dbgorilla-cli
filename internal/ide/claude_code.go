package ide

import (
	"path/filepath"
	"runtime"
)

// ClaudeCode is the Anthropic CLI agent. The preferred MCP-setup path is
// shelling to `claude mcp add` (handles managed-org allowlist policies);
// this adapter exists for the direct-write fallback when the `claude` CLI
// isn't on PATH.
type ClaudeCode struct{}

func (c *ClaudeCode) Name() string { return "Claude Code" }
func (c *ClaudeCode) Slug() string { return "claude-code" }

func (c *ClaudeCode) Detect() bool {
	_, err := findBinary("claude")
	return err == nil
}

func (c *ClaudeCode) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }
func (c *ClaudeCode) DefaultScope() Scope      { return ScopeUser }

// ConfigPath returns the per-scope MCP config path:
//
//   - User: ~/.claude.json (or %APPDATA%\Claude\claude.json on Windows).
//     This is Claude Code's combined settings file -- merge logic in
//     WriteMCPConfig preserves every other top-level key.
//   - Project: .mcp.json in the current working directory. MCP-only file
//     intended to be checked into source control.
func (c *ClaudeCode) ConfigPath(scope Scope) (string, error) {
	switch scope {
	case ScopeProject:
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".mcp.json"), nil
	default: // user
		if runtime.GOOS == "windows" {
			return filepath.Join(homeDir(), "AppData", "Roaming", "Claude", "claude.json"), nil
		}
		return filepath.Join(homeDir(), ".claude.json"), nil
	}
}

func (c *ClaudeCode) TopLevelKey() string { return "mcpServers" }

func (c *ClaudeCode) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}
