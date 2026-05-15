// Package ide provides detection and configuration for IDE/agent MCP
// integrations.
//
// The package centers on three interfaces:
//
//   - Adapter: identity + detection (every supported tool implements this).
//   - Writer:  Adapter + the methods needed to merge an MCP entry into a
//     config file. Tools whose MCP setup we automate.
//   - Hinter:  Adapter + a one-shot Hint() string. Tools we detect but
//     can't auto-configure (e.g. Claude Desktop where remote MCP requires
//     OAuth via Settings -> Connectors). The CLI prints the hint instead.
//
// Adding a new tool means dropping a file in this package, implementing
// either Writer or Hinter, and appending to Registry.
package ide

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scope describes which config layer to write.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// MCPServerName is the key under which dbgorilla registers its MCP server
// in any tool's config. Kept stable across adapters so users see the same
// name in every IDE.
const MCPServerName = "dbgorilla"

// Adapter is the minimal interface every supported tool implements.
type Adapter interface {
	// Name is the human-readable tool name (e.g. "Claude Code").
	Name() string
	// Slug is the CLI flag value (e.g. "claude-code").
	Slug() string
	// Detect returns true when the tool appears installed.
	Detect() bool
}

// Writer is implemented by tools whose MCP config we can write.
type Writer interface {
	Adapter
	// SupportedScopes returns which scopes this tool's config supports.
	// Most tools support {ScopeUser, ScopeProject}; some only support one.
	SupportedScopes() []Scope
	// DefaultScope returns the scope to use when the user passes no flag.
	DefaultScope() Scope
	// ConfigPath returns the absolute path to the MCP config file for the
	// requested scope. Project-scoped paths are resolved against the
	// current working directory.
	ConfigPath(scope Scope) (string, error)
	// TopLevelKey is the JSON key under which MCP servers live in this
	// tool's config (e.g. "mcpServers" for Claude/Cursor/Gemini, "servers"
	// for VS Code, "mcp" for opencode).
	TopLevelKey() string
	// BuildEntry returns the per-tool MCP server entry shape for the
	// dbgorilla server. Different tools have different field names
	// (e.g. Gemini wants "httpUrl", others want "url").
	BuildEntry(mcpURL, apiKey string) map[string]any
}

// Hinter is implemented by tools we detect but can't auto-configure.
// Hint() returns a multi-line string with manual setup instructions.
type Hinter interface {
	Adapter
	Hint(mcpURL string) string
}

// Registry holds every supported tool. Order is preserved -- shows up in
// help text and detection output the same way.
var Registry = []Adapter{
	&ClaudeCode{},
	&ClaudeDesktop{},
	&Cursor{},
	&VSCode{},
	&Opencode{},
	&Gemini{},
}

// Find returns the adapter matching the given slug, or nil.
func Find(slug string) Adapter {
	for _, a := range Registry {
		if a.Slug() == slug {
			return a
		}
	}
	return nil
}

// DetectInstalled returns adapters whose tool is present on the system.
func DetectInstalled() []Adapter {
	var found []Adapter
	for _, a := range Registry {
		if a.Detect() {
			found = append(found, a)
		}
	}
	return found
}

// SupportedSlugs returns all known slugs (writers and hinters), in
// Registry order, for help text.
func SupportedSlugs() []string {
	slugs := make([]string, len(Registry))
	for i, a := range Registry {
		slugs[i] = a.Slug()
	}
	return slugs
}

// WriteResult describes what WriteMCPConfig did. Useful for the caller to
// print accurate user-facing messages ("wrote", "updated", "no change").
type WriteResult struct {
	Path       string
	BackupPath string // empty if no backup needed (fresh file or no-op)
	Created    bool   // true if the config file did not exist before
	Updated    bool   // true if an existing dbgorilla entry was replaced
	NoOp       bool   // true if the existing entry already matched
}

// ErrJSONCRefused is returned when the target config file has a .jsonc
// extension or contains // comments. We refuse to write rather than
// silently destroy the user's comments. Caller should print the entry and
// instruct the user to paste manually.
var ErrJSONCRefused = fmt.Errorf(
	"target config file appears to be JSONC (has // comments); refusing to " +
		"write -- run with --print-config to get the entry to paste manually",
)

// WriteMCPConfig merges the dbgorilla MCP entry into the tool's config
// file at the requested scope. Safety contract:
//
//   - Always reads existing config first; never starts from a blank slate.
//   - Backs up to <path>.backup.<timestamp> (mode 0600) before any write.
//   - Preserves every other top-level key in the file.
//   - Preserves every other entry under the MCP top-level key.
//   - Refuses to write JSONC files (would destroy comments) -- caller
//     should fall back to --print-config.
//   - Idempotent: no write when the existing entry already matches.
//
// Returns the result struct for accurate user messaging.
func WriteMCPConfig(w Writer, mcpURL, apiKey string, scope Scope) (WriteResult, error) {
	res := WriteResult{}
	path, err := w.ConfigPath(scope)
	if err != nil {
		return res, err
	}
	res.Path = path

	if isJSONCPath(path) {
		return res, ErrJSONCRefused
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return res, fmt.Errorf("cannot create directory %s: %w", dir, err)
	}

	entry := w.BuildEntry(mcpURL, apiKey)
	topKey := w.TopLevelKey()

	existing := make(map[string]any)
	data, readErr := os.ReadFile(path)
	switch {
	case os.IsNotExist(readErr):
		res.Created = true
	case readErr != nil:
		return res, fmt.Errorf("cannot read existing config %s: %w", path, readErr)
	default:
		if hasJSONCComments(data) {
			return res, ErrJSONCRefused
		}
		if err := json.Unmarshal(data, &existing); err != nil {
			return res, fmt.Errorf(
				"cannot parse existing config at %s: %w\n"+
					"Refusing to overwrite. Use --print-config to get the entry to paste manually.",
				path, err,
			)
		}
	}

	servers, _ := existing[topKey].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}

	// Idempotency: if our entry exists and already matches what we would
	// write, do nothing. Avoids backup churn on repeated `dbg setup-ide`.
	if prior, ok := servers[MCPServerName]; ok {
		if jsonEqual(prior, entry) {
			res.NoOp = true
			return res, nil
		}
		res.Updated = true
	}

	// Backup before any mutation, even if the file existed but we're
	// about to no-op'd above (we only get here if the entry differs).
	if !res.Created {
		backupPath := fmt.Sprintf("%s.backup.%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backupPath, data, 0600); err != nil {
			return res, fmt.Errorf("cannot write backup to %s: %w", backupPath, err)
		}
		_ = os.Chmod(backupPath, 0600)
		res.BackupPath = backupPath
	}

	servers[MCPServerName] = entry
	existing[topKey] = servers

	merged, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return res, fmt.Errorf("cannot serialize merged config: %w", err)
	}
	// 0600 because the merged file contains the dbgorilla bearer token.
	if err := os.WriteFile(path, append(merged, '\n'), 0600); err != nil {
		return res, fmt.Errorf("cannot write config to %s: %w", path, err)
	}
	_ = os.Chmod(path, 0600)
	return res, nil
}

// isJSONCPath returns true if the path ends in .jsonc.
func isJSONCPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".jsonc")
}

// hasJSONCComments scans file bytes for // line comments or /* */ block
// comments outside of string literals. We don't need a perfect parser;
// any false positive just routes the user to --print-config which is the
// safe fallback.
func hasJSONCComments(data []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	inString := false
	escape := false
	for scanner.Scan() {
		line := scanner.Text()
		for i := 0; i < len(line); i++ {
			c := line[i]
			if escape {
				escape = false
				continue
			}
			switch {
			case c == '\\':
				escape = true
			case c == '"':
				inString = !inString
			case !inString && c == '/' && i+1 < len(line) && (line[i+1] == '/' || line[i+1] == '*'):
				return true
			}
		}
	}
	return false
}

// jsonEqual compares two values via JSON-canonicalised round-trip. Slow
// but correct for the small entry maps we compare here. Avoids reflect
// edge cases with map[string]any vs map[string]string nesting.
func jsonEqual(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(canonicaliseJSON(ab), canonicaliseJSON(bb))
}

func canonicaliseJSON(in []byte) []byte {
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	out, err := json.Marshal(v)
	if err != nil {
		return in
	}
	return out
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
