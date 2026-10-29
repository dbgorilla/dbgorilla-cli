package ide

import (
	"os"
	"path/filepath"
)

// VSCode writes the Copilot user file, $COPILOT_HOME/mcp-config.json or
// ~/.copilot/mcp-config.json. VS Code's add-server flow labels its own
// mcp.json files (workspace .vscode/mcp.json and the user-profile mcp.json)
// deprecated, and reads the Copilot file through GitHub Copilot CLI
// discovery, which is on by default. The Copilot file is the portable
// format: top-level "mcpServers", not VS Code's "servers".
//
// There is no project scope: the only non-deprecated workspace destination
// (workspace-root .mcp.json) sits behind an experimental VS Code setting.
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

func (v *VSCode) SupportedScopes() []Scope { return []Scope{ScopeUser} }
func (v *VSCode) DefaultScope() Scope      { return ScopeUser }

// ConfigPath is the same on every OS. An empty COPILOT_HOME counts as unset.
func (v *VSCode) ConfigPath(scope Scope) (string, error) {
	if dir := os.Getenv("COPILOT_HOME"); dir != "" {
		return filepath.Join(dir, "mcp-config.json"), nil
	}
	return filepath.Join(homeDir(), ".copilot", "mcp-config.json"), nil
}

func (v *VSCode) TopLevelKey() string { return "mcpServers" }

func (v *VSCode) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}

// LegacyConfigs are the files earlier versions of this CLI wrote. They keep
// working in VS Code, so an entry left there shows DBGorilla twice.
func (v *VSCode) LegacyConfigs() []Writer {
	return []Writer{&vscodeLegacy{scope: ScopeProject}, &vscodeLegacy{scope: ScopeUser}}
}

// vscodeLegacy addresses VS Code's own mcp.json files, so --remove can clean
// them with the same merge contract as every other write.
type vscodeLegacy struct{ scope Scope }

func (l *vscodeLegacy) Name() string             { return "VS Code (mcp.json)" }
func (l *vscodeLegacy) Slug() string             { return "vscode-legacy" }
func (l *vscodeLegacy) Detect() bool             { return false }
func (l *vscodeLegacy) SupportedScopes() []Scope { return []Scope{l.scope} }
func (l *vscodeLegacy) DefaultScope() Scope      { return l.scope }
func (l *vscodeLegacy) TopLevelKey() string      { return "servers" }
func (l *vscodeLegacy) BuildEntry(u, k string) map[string]any {
	return (&VSCode{}).BuildEntry(u, k)
}

// ConfigPath ignores its argument: each legacy writer is bound to one file.
func (l *vscodeLegacy) ConfigPath(Scope) (string, error) {
	if l.scope == ScopeProject {
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
