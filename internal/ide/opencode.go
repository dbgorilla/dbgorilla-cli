package ide

import "path/filepath"

// Opencode is sst/opencode -- TUI agent. MCP config lives inside the
// shared opencode.json under the "mcp" key; entries take type:"remote"
// for HTTP servers.
type Opencode struct{}

func (o *Opencode) Name() string { return "opencode" }
func (o *Opencode) Slug() string { return "opencode" }

func (o *Opencode) Detect() bool {
	_, err := findBinary("opencode")
	return err == nil
}

func (o *Opencode) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }
func (o *Opencode) DefaultScope() Scope      { return ScopeUser }

// ConfigPath:
//   - User: ~/.config/opencode/opencode.json
//   - Project: opencode.json in cwd
//
// Note: opencode also accepts opencode.jsonc with comments. We deliberately
// target the .json variant for writes and let the JSONC-refusal logic in
// WriteMCPConfig redirect users to --print-config if they're using JSONC.
func (o *Opencode) ConfigPath(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, "opencode.json"), nil
	}
	return filepath.Join(homeDir(), ".config", "opencode", "opencode.json"), nil
}

func (o *Opencode) TopLevelKey() string { return "mcp" }

func (o *Opencode) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"type":    "remote",
		"url":     mcpURL,
		"enabled": true,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}
