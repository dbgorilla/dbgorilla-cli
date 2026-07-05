package ide

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file extends ide_test.go and adapters_test.go. It targets the paths
// those files leave uncovered: adapter identity getters, detection seams
// (PATH + HOME controlled, never touching a real IDE), per-scope config
// paths for every adapter, full-round-trip writes for each real Writer, and
// the error branches of WriteMCPConfig / the JSONC + jsonEqual helpers.
//
// Hermetic contract: detection is driven only through t.Setenv("PATH", ...)
// (a temp dir holding fake executables) and t.Setenv("HOME", ...) (a temp
// dir where we materialise fake *.app bundles). No real binary or real HOME
// is consulted for an assertion.

// withFakeBinaries points PATH at a fresh temp dir containing an executable
// stub for each named tool, so exec.LookPath (via findBinary) resolves it.
func withFakeBinaries(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake binary %s: %v", n, err)
		}
		// Defend against a restrictive umask stripping the exec bits.
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod fake binary %s: %v", n, err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// emptyPATH points PATH at an empty temp dir so no tool resolves.
func emptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// --- Identity getters -----------------------------------------------------

func TestAdapters_NameAndSlug(t *testing.T) {
	cases := []struct {
		a    Adapter
		name string
		slug string
	}{
		{&ClaudeCode{}, "Claude Code", "claude-code"},
		{&ClaudeDesktop{}, "Claude Desktop", "claude-desktop"},
		{&Cursor{}, "Cursor", "cursor"},
		{&VSCode{}, "VS Code", "vscode"},
		{&Opencode{}, "opencode", "opencode"},
		{&Gemini{}, "Gemini CLI", "gemini"},
	}
	for _, c := range cases {
		if got := c.a.Name(); got != c.name {
			t.Errorf("%T Name() = %q, want %q", c.a, got, c.name)
		}
		if got := c.a.Slug(); got != c.slug {
			t.Errorf("%T Slug() = %q, want %q", c.a, got, c.slug)
		}
	}
}

func TestWriters_DefaultScope(t *testing.T) {
	cases := []struct {
		w    Writer
		want Scope
	}{
		{&ClaudeCode{}, ScopeUser},
		{&Cursor{}, ScopeUser},
		{&VSCode{}, ScopeProject}, // VS Code's MCP UX is workspace-first.
		{&Opencode{}, ScopeUser},
		{&Gemini{}, ScopeUser},
	}
	for _, c := range cases {
		if got := c.w.DefaultScope(); got != c.want {
			t.Errorf("%s DefaultScope() = %q, want %q", c.w.Slug(), got, c.want)
		}
	}
}

// --- Detection: binary-only adapters (deterministic true AND false) -------

func TestBinaryOnlyAdapters_Detect(t *testing.T) {
	cases := []struct {
		a   Adapter
		bin string
	}{
		{&ClaudeCode{}, "claude"},
		{&Gemini{}, "gemini"},
		{&Opencode{}, "opencode"},
	}

	t.Run("absent", func(t *testing.T) {
		emptyPATH(t)
		for _, c := range cases {
			if c.a.Detect() {
				t.Errorf("%T Detect() = true with empty PATH, want false", c.a)
			}
		}
	})

	for _, c := range cases {
		t.Run(c.bin+"_on_path", func(t *testing.T) {
			withFakeBinaries(t, c.bin)
			if !c.a.Detect() {
				t.Errorf("%T Detect() = false with %q on PATH, want true", c.a, c.bin)
			}
		})
	}
}

// --- Detection: app-bundle adapters (via binary, then via ~/Applications) --

func TestAppBundleAdapters_DetectViaBinary(t *testing.T) {
	t.Run("cursor", func(t *testing.T) {
		withFakeBinaries(t, "cursor")
		if !(&Cursor{}).Detect() {
			t.Error("Cursor.Detect() = false with cursor on PATH, want true")
		}
	})
	t.Run("vscode", func(t *testing.T) {
		withFakeBinaries(t, "code")
		if !(&VSCode{}).Detect() {
			t.Error("VSCode.Detect() = false with code on PATH, want true")
		}
	})
}

// materializeClaudeDesktop creates a fake Claude Desktop presence at its actual
// per-OS detect path under the temp home, so ClaudeDesktop.Detect() returns
// true regardless of GOOS. Its detect paths differ by OS (~/Applications/
// Claude.app on darwin, ~/.config/Claude on linux, ~/AppData/Local/
// AnthropicClaude on windows), so a hardcoded darwin path fails on CI (linux).
func materializeClaudeDesktop(t *testing.T, home string) {
	t.Helper()
	made := false
	for _, p := range claudeDesktopAppPaths() {
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("mkdir fake Claude Desktop path %s: %v", p, err)
			}
			made = true
		}
	}
	if !made {
		t.Fatalf("no home-rooted Claude Desktop detect path under %s (paths=%v)", home, claudeDesktopAppPaths())
	}
}

// TestAppBundleAdapters_DetectViaHomeBundle materialises fake IDE presence under
// a temp HOME and clears PATH. Detect must return true off the home artifact
// alone. Cursor/VS Code check ~/Applications/<App>.app on every OS; Claude
// Desktop's home path is per-OS (see materializeClaudeDesktop). Hermetic
// regardless of what the real machine has installed.
func TestAppBundleAdapters_DetectViaHomeBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	emptyPATH(t)

	for _, app := range []string{"Cursor.app", "Visual Studio Code.app"} {
		if err := os.MkdirAll(filepath.Join(home, "Applications", app), 0o755); err != nil {
			t.Fatalf("mkdir fake app %s: %v", app, err)
		}
	}
	materializeClaudeDesktop(t, home)

	if !(&Cursor{}).Detect() {
		t.Error("Cursor.Detect() = false with ~/Applications/Cursor.app present")
	}
	if !(&VSCode{}).Detect() {
		t.Error("VSCode.Detect() = false with ~/Applications bundle present")
	}
	if !(&ClaudeDesktop{}).Detect() {
		t.Error("ClaudeDesktop.Detect() = false with ~/Applications/Claude.app present")
	}
}

// --- Registry-level helpers ----------------------------------------------

func TestSupportedSlugs_MatchesRegistryOrder(t *testing.T) {
	got := SupportedSlugs()
	want := []string{"claude-code", "claude-desktop", "cursor", "vscode", "opencode", "gemini"}
	if len(got) != len(want) {
		t.Fatalf("SupportedSlugs() len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SupportedSlugs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetectInstalled_ReportsEveryPresentTool(t *testing.T) {
	// Make every tool resolvable: fake binaries for CLI-detected tools and a
	// temp HOME holding every *.app bundle for the app-detected tools. This
	// keeps the result independent of the real machine.
	home := t.TempDir()
	t.Setenv("HOME", home)
	withFakeBinaries(t, "claude", "gemini", "opencode", "cursor", "code")
	for _, app := range []string{"Cursor.app", "Visual Studio Code.app"} {
		if err := os.MkdirAll(filepath.Join(home, "Applications", app), 0o755); err != nil {
			t.Fatalf("mkdir fake app %s: %v", app, err)
		}
	}
	materializeClaudeDesktop(t, home)

	found := DetectInstalled()
	if len(found) != len(Registry) {
		t.Fatalf("DetectInstalled() found %d tools, want all %d", len(found), len(Registry))
	}
	slugs := map[string]bool{}
	for _, a := range found {
		slugs[a.Slug()] = true
	}
	for _, a := range Registry {
		if !slugs[a.Slug()] {
			t.Errorf("DetectInstalled() missing %q", a.Slug())
		}
	}
}

// --- ConfigPath: both scopes for every adapter ----------------------------

func TestAdapters_ConfigPath_BothScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		w          Writer
		userSuffix string // filepath suffix (may be OS-specific for VS Code)
		userSubstr string // if set, assert Contains instead of HasSuffix
		projSuffix string
	}{
		{w: &ClaudeCode{}, userSuffix: ".claude.json", projSuffix: ".mcp.json"},
		{w: &Cursor{}, userSuffix: filepath.Join(".cursor", "mcp.json"), projSuffix: filepath.Join(".cursor", "mcp.json")},
		{w: &VSCode{}, userSubstr: filepath.Join("Code", "User", "mcp.json"), projSuffix: filepath.Join(".vscode", "mcp.json")},
		{w: &Opencode{}, userSuffix: filepath.Join("opencode", "opencode.json"), projSuffix: "opencode.json"},
		{w: &Gemini{}, userSuffix: filepath.Join(".gemini", "settings.json"), projSuffix: filepath.Join(".gemini", "settings.json")},
	}
	for _, c := range cases {
		t.Run(c.w.Slug(), func(t *testing.T) {
			user, err := c.w.ConfigPath(ScopeUser)
			if err != nil {
				t.Fatalf("ConfigPath(user): %v", err)
			}
			switch {
			case c.userSubstr != "":
				if !strings.Contains(user, c.userSubstr) {
					t.Errorf("user path %q does not contain %q", user, c.userSubstr)
				}
			default:
				if !strings.HasSuffix(user, c.userSuffix) {
					t.Errorf("user path %q missing suffix %q", user, c.userSuffix)
				}
				if !strings.HasPrefix(user, home) {
					t.Errorf("user path %q not rooted under temp HOME %q", user, home)
				}
			}

			proj, err := c.w.ConfigPath(ScopeProject)
			if err != nil {
				t.Fatalf("ConfigPath(project): %v", err)
			}
			if !strings.HasSuffix(proj, c.projSuffix) {
				t.Errorf("project path %q missing suffix %q", proj, c.projSuffix)
			}
		})
	}
}

// Note on the getCWD error branch: the project-scoped ConfigPath methods'
// `if err != nil { return "", err }` (and getCWD's own error return) cannot be
// exercised hermetically on darwin. macOS's getcwd(3) keeps resolving the
// working directory even after it is unlinked or an ancestor is made
// unreadable, so os.Getwd never errors in a controllable way. Left uncovered
// by design rather than with an always-skipping test.

// --- Full round-trip writes for every real Writer ------------------------

func TestWriteMCPConfig_RoundTripAllWriters_UserScope(t *testing.T) {
	const url = "https://api.dbgorilla.com/mcp/"
	const key = "tok-abc123"

	for _, w := range []Writer{&ClaudeCode{}, &Cursor{}, &VSCode{}, &Opencode{}, &Gemini{}} {
		t.Run(w.Slug(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			res, err := WriteMCPConfig(w, url, key, ScopeUser)
			if err != nil {
				t.Fatalf("WriteMCPConfig: %v", err)
			}
			if !res.Created {
				t.Errorf("Created = false on fresh file")
			}
			path, _ := w.ConfigPath(ScopeUser)
			if res.Path != path {
				t.Errorf("res.Path = %q, want %q", res.Path, path)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("unmarshal written config: %v\n%s", err, data)
			}
			servers, ok := cfg[w.TopLevelKey()].(map[string]any)
			if !ok {
				t.Fatalf("top-level key %q missing/not an object: %s", w.TopLevelKey(), data)
			}
			entry, ok := servers[MCPServerName]
			if !ok {
				t.Fatalf("no %q entry under %q: %s", MCPServerName, w.TopLevelKey(), data)
			}
			// The written entry must be exactly what BuildEntry produced --
			// this is the load-bearing per-adapter shape (httpUrl vs url,
			// remote vs http type, etc.), not just "some entry exists".
			if !jsonEqual(entry, w.BuildEntry(url, key)) {
				t.Errorf("written entry != BuildEntry\n got: %#v\nwant: %#v", entry, w.BuildEntry(url, key))
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Errorf("mode = %o, want 0600 (contains bearer token)", info.Mode().Perm())
			}
		})
	}
}

// TestWriteMCPConfig_ProjectScope_MergePreservesNeighbours writes a real
// project-scoped config (Cursor) alongside a pre-existing unrelated server,
// then asserts the neighbour survives. Uses chdir into a temp dir so the
// project path resolves hermetically.
func TestWriteMCPConfig_ProjectScope_MergePreservesNeighbours(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	work := t.TempDir()
	if err := os.Chdir(work); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	c := &Cursor{}
	path, err := c.ConfigPath(ScopeProject)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pre := map[string]any{
		"mcpServers": map[string]any{
			"legacy": map[string]any{"url": "https://legacy.example/mcp/"},
		},
	}
	preBytes, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(path, preBytes, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	res, err := WriteMCPConfig(c, "https://api/mcp/", "k", ScopeProject)
	if err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}
	if res.Created {
		t.Errorf("Created = true, want false (file pre-existed)")
	}
	if res.BackupPath == "" {
		t.Errorf("expected a backup when mutating an existing file")
	}

	data, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if servers["legacy"] == nil {
		t.Error("legacy server was clobbered by the merge")
	}
	if servers[MCPServerName] == nil {
		t.Error("dbgorilla entry not added")
	}
}

// --- WriteMCPConfig error branches ---------------------------------------

// configPathErrWriter reuses stubWriter's behaviour but fails ConfigPath.
type configPathErrWriter struct{ stubWriter }

func (e *configPathErrWriter) ConfigPath(Scope) (string, error) {
	return "", fmt.Errorf("no config path available")
}

func TestWriteMCPConfig_ConfigPathError(t *testing.T) {
	_, err := WriteMCPConfig(&configPathErrWriter{}, "https://api/mcp/", "k", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "no config path available") {
		t.Fatalf("expected ConfigPath error to propagate, got: %v", err)
	}
}

func TestWriteMCPConfig_MkdirAllError(t *testing.T) {
	dir := t.TempDir()
	// A regular file stands where a parent directory is expected, so
	// MkdirAll must fail.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "config.json")
	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "k", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "cannot create directory") {
		t.Fatalf("expected MkdirAll error, got: %v", err)
	}
}

func TestWriteMCPConfig_ReadErrorWhenPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config-as-dir")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// ReadFile on a directory returns a non-IsNotExist error, hitting the
	// "cannot read existing config" branch.
	_, err := WriteMCPConfig(&stubWriter{configPath: asDir}, "https://api/mcp/", "k", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "cannot read existing config") {
		t.Fatalf("expected read error for directory path, got: %v", err)
	}
}

func TestWriteMCPConfig_TopKeyHoldsNonObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// mcpServers is a string, not an object -- the merge must recover by
	// starting a fresh servers map rather than panicking.
	if err := os.WriteFile(path, []byte(`{"mcpServers":"oops","keep":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://api/mcp/", "k", ScopeUser)
	if err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}
	if res.Created {
		t.Errorf("Created = true, want false (file existed)")
	}
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers not rebuilt as object: %s", data)
	}
	if servers[MCPServerName] == nil {
		t.Error("dbgorilla entry not written")
	}
	if cfg["keep"] != true {
		t.Error("unrelated top-level key was lost")
	}
}

func TestWriteMCPConfig_BackupWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based error path is meaningless for root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Existing config with a differing dbgorilla entry so a backup is needed.
	seed := `{"mcpServers":{"dbgorilla":{"type":"http","url":"https://old/mcp/","headers":{"Authorization":"Bearer old"}}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore writability before TempDir cleanup runs (LIFO: this runs first).
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: can read file, cannot create backup
		t.Fatal(err)
	}

	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://new/mcp/", "new", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "cannot write backup") {
		t.Fatalf("expected backup write error, got: %v", err)
	}
}

func TestWriteMCPConfig_FinalWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based error path is meaningless for root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"mcpServers":{"dbgorilla":{"type":"http","url":"https://old/mcp/","headers":{"Authorization":"Bearer old"}}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	// Dir stays writable (backup succeeds) but the target file is read-only,
	// so the final os.WriteFile(path, ...) fails.
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}

	_, err := WriteMCPConfig(&stubWriter{configPath: path}, "https://new/mcp/", "new", ScopeUser)
	if err == nil || !strings.Contains(err.Error(), "cannot write config") {
		t.Fatalf("expected final write error, got: %v", err)
	}
}

// --- Helper: hasJSONCComments edge cases ----------------------------------

func TestHasJSONCComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain object", `{"a":1,"b":"two"}`, false},
		{"line comment", "{\n  // hi\n  \"a\":1\n}", true},
		{"block comment", `{"a":1} /* trailing */`, true},
		{"slashes inside string are not comments", `{"url":"http://x//y"}`, false},
		{"escaped backslash before quote", `{"path":"C:\\Users\\me"}`, false},
		{"escaped quote inside string", `{"a":"he said \"//\" loudly"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasJSONCComments([]byte(c.in)); got != c.want {
				t.Errorf("hasJSONCComments(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// --- Helper: jsonEqual / canonicaliseJSON error branches ------------------

func TestJSONEqual(t *testing.T) {
	if !jsonEqual(map[string]any{"a": 1}, map[string]any{"a": 1}) {
		t.Error("equal maps reported unequal")
	}
	if jsonEqual(map[string]any{"a": 1}, map[string]any{"a": 2}) {
		t.Error("differing maps reported equal")
	}
	// A channel cannot be marshalled -- exercises both marshal-error returns.
	ch := make(chan int)
	if jsonEqual(ch, map[string]any{}) {
		t.Error("unmarshalable first arg should compare unequal")
	}
	if jsonEqual(map[string]any{}, ch) {
		t.Error("unmarshalable second arg should compare unequal")
	}
}

func TestCanonicaliseJSON_InvalidInputReturnedVerbatim(t *testing.T) {
	in := []byte("this is not json")
	if got := canonicaliseJSON(in); string(got) != string(in) {
		t.Errorf("canonicaliseJSON(invalid) = %q, want input returned verbatim", got)
	}
}

// --- Helpers: findBinary / exists ----------------------------------------

func TestFindBinary(t *testing.T) {
	withFakeBinaries(t, "mytool")
	if _, err := findBinary("mytool"); err != nil {
		t.Errorf("findBinary(mytool) = %v, want found", err)
	}
	if _, err := findBinary("definitely-not-a-real-binary-xyz"); err == nil {
		t.Error("findBinary(missing) = nil error, want lookup failure")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !exists(f) {
		t.Error("exists(file) = false, want true")
	}
	if !exists(dir) {
		t.Error("exists(dir) = false, want true")
	}
	if exists(filepath.Join(dir, "missing")) {
		t.Error("exists(missing) = true, want false")
	}
}
