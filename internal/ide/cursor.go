package ide

import "path/filepath"

// Cursor is the AI-first VS Code fork. Its MCP config is a dedicated
// JSON file (mcp.json), not part of the larger Cursor settings.
type Cursor struct{}

func (c *Cursor) Name() string { return "Cursor" }
func (c *Cursor) Slug() string { return "cursor" }

func (c *Cursor) Detect() bool {
	if _, err := findBinary("cursor"); err == nil {
		return true
	}
	// macOS app bundle is a strong secondary signal -- many users never
	// install the `cursor` CLI helper.
	for _, p := range []string{
		"/Applications/Cursor.app",
		filepath.Join(homeDir(), "Applications", "Cursor.app"),
	} {
		if exists(p) {
			return true
		}
	}
	return false
}

func (c *Cursor) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }
func (c *Cursor) DefaultScope() Scope      { return ScopeUser }

// ConfigPath:
//   - User: ~/.cursor/mcp.json
//   - Project: .cursor/mcp.json (in the current working directory)
func (c *Cursor) ConfigPath(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".cursor", "mcp.json"), nil
	}
	return filepath.Join(homeDir(), ".cursor", "mcp.json"), nil
}

func (c *Cursor) TopLevelKey() string { return "mcpServers" }

func (c *Cursor) BuildEntry(mcpURL, apiKey string) map[string]any {
	// Cursor uses URL-only entries to select HTTP/SSE transport
	// automatically. Adding `type: "http"` is harmless on current versions
	// but URL is the load-bearing field.
	return map[string]any{
		"url": mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}
