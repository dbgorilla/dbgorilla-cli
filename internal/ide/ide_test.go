package ide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubWriter implements Writer for unit-testing WriteMCPConfig with a
// configurable top-level key (so we can exercise the VS Code "servers"
// path as well as the default "mcpServers").
type stubWriter struct {
	configPath string
	topKey     string
}

func (s *stubWriter) Name() string                    { return "Stub" }
func (s *stubWriter) Slug() string                    { return "stub" }
func (s *stubWriter) Detect() bool                    { return true }
func (s *stubWriter) SupportedScopes() []Scope        { return []Scope{ScopeUser} }
func (s *stubWriter) DefaultScope() Scope             { return ScopeUser }
func (s *stubWriter) ConfigPath(_ Scope) (string, error) { return s.configPath, nil }
func (s *stubWriter) TopLevelKey() string {
	if s.topKey == "" {
		return "mcpServers"
	}
	return s.topKey
}
func (s *stubWriter) BuildEntry(url, apiKey string) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  url,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}

// --- Fresh file path -------------------------------------------------------

func TestWriteMCPConfig_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	w := &stubWriter{configPath: path}

	res, err := WriteMCPConfig(w, "https://api/mcp/", "key123", ScopeUser)
	if err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}
	if !res.Created {
		t.Errorf("Created=false, want true on fresh file")
	}
	if res.BackupPath != "" {
		t.Errorf("unexpected backup on fresh file: %s", res.BackupPath)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("mode = %o, want 0600", mode)
	}
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if servers["dbgorilla"] == nil {
		t.Errorf("dbgorilla entry missing: %+v", servers)
	}
}

// --- Merge: preserve unrelated entries ------------------------------------

func TestWriteMCPConfig_PreservesOtherServersAndCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	existing := map[string]any{
		"theme": "dark",
		"mcpServers": map[string]any{
			"other-server": map[string]any{
				"type":    "stdio",
				"command": "/usr/bin/other",
			},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	res, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "key123", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath == "" {
		t.Error("expected backup, got none")
	}

	got, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if servers["other-server"] == nil {
		t.Error("other-server was clobbered")
	}
	if servers["dbgorilla"] == nil {
		t.Error("dbgorilla entry not added")
	}
	if cfg["theme"] != "dark" {
		t.Error("top-level theme key was lost")
	}
}

// --- Idempotency: matching entry => no-op, no backup ----------------------

func TestWriteMCPConfig_IdempotentNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	w := &stubWriter{configPath: path}

	first, err := WriteMCPConfig(w, "https://api/mcp/", "key123", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatal("first write should be Created")
	}

	second, err := WriteMCPConfig(w, "https://api/mcp/", "key123", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NoOp {
		t.Errorf("second identical write should be NoOp, got %+v", second)
	}
	if second.BackupPath != "" {
		t.Errorf("no-op should not create backup, got %s", second.BackupPath)
	}

	// Verify no .backup.* files exist.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup.") {
			t.Errorf("unexpected backup file from idempotent re-run: %s", e.Name())
		}
	}
}

// --- Updated: matching key, different value => Updated + backup -----------

func TestWriteMCPConfig_UpdatedReplacesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	w := &stubWriter{configPath: path}

	if _, err := WriteMCPConfig(w, "https://api/mcp/", "old-key", ScopeUser); err != nil {
		t.Fatal(err)
	}
	res, err := WriteMCPConfig(w, "https://api/mcp/", "NEW-key", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated {
		t.Errorf("expected Updated=true, got %+v", res)
	}
	if res.BackupPath == "" {
		t.Error("Updated should produce a backup")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "NEW-key") {
		t.Errorf("rewritten file missing new key: %s", got)
	}
}

// --- VS Code top-level key path ------------------------------------------

func TestWriteMCPConfig_RespectsTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	w := &stubWriter{configPath: path, topKey: "servers"} // VS Code style

	if _, err := WriteMCPConfig(w, "https://api/mcp/", "k", ScopeUser); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)
	if _, ok := cfg["servers"].(map[string]any); !ok {
		t.Errorf("expected top-level 'servers' key, got: %s", data)
	}
	if _, ok := cfg["mcpServers"]; ok {
		t.Errorf("must NOT add 'mcpServers' when topKey=servers; got: %s", data)
	}
}

// --- Malformed config => bail clean, don't clobber -----------------------

func TestWriteMCPConfig_MalformedJSONIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "k", ScopeUser)
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{not-json" {
		t.Errorf("malformed file was overwritten: %q", string(data))
	}
}

// --- JSONC refusal: extension and inline comments -------------------------

func TestWriteMCPConfig_RefusesJSONCExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "k", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "JSONC") {
		t.Errorf("expected JSONC refusal, got: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("must not create the file when refusing JSONC")
	}
}

func TestWriteMCPConfig_RefusesInlineComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
  // user added this comment
  "mcpServers": {}
}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "k", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "JSONC") {
		t.Errorf("expected JSONC refusal on inline comments, got: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("comment-bearing file was modified: %s", got)
	}
}

// --- Detection of registered adapters -------------------------------------

func TestRegistry_AllSlugsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Registry {
		if seen[a.Slug()] {
			t.Errorf("duplicate slug in registry: %s", a.Slug())
		}
		seen[a.Slug()] = true
	}
}

func TestFind_KnownAndUnknown(t *testing.T) {
	if a := Find("claude-code"); a == nil {
		t.Error("expected to find claude-code")
	}
	if a := Find("nonexistent"); a != nil {
		t.Error("expected nil for unknown slug")
	}
}
