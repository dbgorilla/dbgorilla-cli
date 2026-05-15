package ide

import "path/filepath"

// Gemini is Google's gemini-cli. MCP config lives in settings.json under
// "mcpServers". For Streamable HTTP transport the entry uses "httpUrl"
// (not "url" -- that selects the deprecated SSE transport).
type Gemini struct{}

func (g *Gemini) Name() string { return "Gemini CLI" }
func (g *Gemini) Slug() string { return "gemini" }

func (g *Gemini) Detect() bool {
	_, err := findBinary("gemini")
	return err == nil
}

func (g *Gemini) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }
func (g *Gemini) DefaultScope() Scope      { return ScopeUser }

// ConfigPath:
//   - User: ~/.gemini/settings.json
//   - Project: .gemini/settings.json in cwd
func (g *Gemini) ConfigPath(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".gemini", "settings.json"), nil
	}
	return filepath.Join(homeDir(), ".gemini", "settings.json"), nil
}

func (g *Gemini) TopLevelKey() string { return "mcpServers" }

func (g *Gemini) BuildEntry(mcpURL, apiKey string) map[string]any {
	// httpUrl (NOT url) selects the Streamable HTTP transport. The "url"
	// field would route to the deprecated SSE transport.
	return map[string]any{
		"httpUrl": mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
		"timeout": 30000,
	}
}
