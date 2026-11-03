package ide

import (
	"path/filepath"
)

// VSCode targets the official MCP support shipped in mid-2025. Its config
// file uses the top-level key "servers" -- NOT "mcpServers". This is the
// most common copy-paste mistake when wiring up MCP across IDEs.
type VSCode struct{}

func (v *VSCode) Name() string { return "VS Code" }
func (v *VSCode) Slug() string { return "vscode" }

func (v *VSCode) Detect() bool {
	if _, err := findBinary("code"); err == nil {
		return true
	}
	// macOS app bundle as a fallback signal.
	for _, p := range []string{
		"/Applications/Visual Studio Code.app",
		filepath.Join(homeDir(), "Applications", "Visual Studio Code.app"),
	} {
		if exists(p) {
			return true
		}
	}
	return false
}

func (v *VSCode) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }

// DefaultScope: project. VS Code's MCP UX is built around .vscode/mcp.json
// living next to the workspace -- that's the conventional place. Users who
// want global setup pass --scope user explicitly.
func (v *VSCode) DefaultScope() Scope { return ScopeProject }

// ConfigPath:
//   - Project: .vscode/mcp.json (in cwd)
//   - User:    OS-specific Code/User/mcp.json
func (v *VSCode) ConfigPath(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".vscode", "mcp.json"), nil
	}
	switch goos {
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Application Support", "Code", "User", "mcp.json"), nil
	case "windows":
		return filepath.Join(homeDir(), "AppData", "Roaming", "Code", "User", "mcp.json"), nil
	default:
		return filepath.Join(homeDir(), ".config", "Code", "User", "mcp.json"), nil
	}
}

// TopLevelKey is "servers", not "mcpServers". This is the load-bearing
// difference between VS Code and every other IDE in this package.
func (v *VSCode) TopLevelKey() string { return "servers" }

func (v *VSCode) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}

// SkillDir: ~/.copilot/skills/dbgorilla (user) or .github/skills/dbgorilla
// (project). The project root is .github, not .vscode: skills belong to
// Copilot rather than to the editor, and that is where both the editor's
// local sessions and the Copilot coding agent look for them.
func (v *VSCode) SkillDir(scope Scope) (string, error) {
	return skillDir(scope, []string{".copilot", "skills"}, []string{".github", "skills"})
}
