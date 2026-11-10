package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VS Code moved from its own mcp.json files to the Copilot user file. These
// pin where setup-ide writes now, and what it does about entries earlier
// versions left behind.
func TestRunSetupIDE_VSCode(t *testing.T) {
	setupVSCode := func(t *testing.T) string {
		t.Helper()
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "vscode")
		return capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("setup: %v", err)
			}
		})
	}
	removeVSCode := func(t *testing.T) string {
		t.Helper()
		c := setupIDETestCmd()
		_ = c.Flags().Set("client", "vscode")
		_ = c.Flags().Set("remove", "true")
		return capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("remove: %v", err)
			}
		})
	}
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Linux paths: the tests set HOME, and the user-profile legacy file on
	// Linux is the only one rooted directly under it.
	legacyUser := func(home string) string {
		return filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}

	t.Run("writes mcpServers to ~/.copilot/mcp-config.json", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		setupVSCode(t)
		data, err := os.ReadFile(filepath.Join(home, ".copilot", "mcp-config.json"))
		if err != nil {
			t.Fatalf("no Copilot config written: %v", err)
		}
		var cfg map[string]map[string]map[string]any
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, data)
		}
		entry := cfg["mcpServers"]["dbgorilla"]
		if entry["type"] != "http" || !strings.HasSuffix(entry["url"].(string), "/mcp/") {
			t.Errorf("entry = %v", entry)
		}
		if _, err := os.Stat(filepath.Join(".vscode", "mcp.json")); !os.IsNotExist(err) {
			t.Errorf(".vscode/mcp.json was written (err=%v)", err)
		}
	})

	t.Run("--scope project still writes the Copilot user file", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "vscode")
		mustSet(t, c, "scope", "project")
		capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("setup: %v", err)
			}
		})
		if _, err := os.Stat(filepath.Join(home, ".copilot", "mcp-config.json")); err != nil {
			t.Errorf("Copilot config missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(".vscode", "mcp.json")); !os.IsNotExist(err) {
			t.Errorf(".vscode/mcp.json was written (err=%v)", err)
		}
	})

	t.Run("names an old entry and leaves the old file alone", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		old := `{"servers":{"dbgorilla":{"type":"http","url":"https://x/mcp/"}}}`
		write(t, filepath.Join(".vscode", "mcp.json"), old)
		out := setupVSCode(t)
		if !strings.Contains(out, filepath.Join(".vscode", "mcp.json")) ||
			!strings.Contains(out, "older dbgorilla entry") {
			t.Errorf("no hint about the old entry:\n%s", out)
		}
		if data, _ := os.ReadFile(filepath.Join(".vscode", "mcp.json")); string(data) != old {
			t.Errorf("old file was modified:\n%s", data)
		}
	})

	t.Run("no hint when there is no old entry", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		if out := setupVSCode(t); strings.Contains(out, "older dbgorilla entry") {
			t.Errorf("unexpected hint:\n%s", out)
		}
	})

	t.Run("--remove cleans the new file and old ones, keeping other servers", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		setupVSCode(t)
		write(t, filepath.Join(".vscode", "mcp.json"),
			`{"servers":{"dbgorilla":{},"other":{"type":"http","url":"https://o/"}}}`)
		write(t, legacyUser(home), `{"servers":{"dbgorilla":{}}}`)

		out := removeVSCode(t)
		for _, p := range []string{
			filepath.Join(home, ".copilot", "mcp-config.json"),
			filepath.Join(".vscode", "mcp.json"),
			legacyUser(home),
		} {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			if strings.Contains(string(data), "dbgorilla") {
				t.Errorf("entry survived in %s:\n%s", p, data)
			}
		}
		if data, _ := os.ReadFile(filepath.Join(".vscode", "mcp.json")); !strings.Contains(string(data), `"other"`) {
			t.Errorf("another server was removed:\n%s", data)
		}
		if strings.Count(out, "Removed the dbgorilla entry") != 3 {
			t.Errorf("want three removals reported:\n%s", out)
		}
	})

	t.Run("--remove asks for a hand edit on a commented old file", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		body := "// mine\n{\"servers\":{\"dbgorilla\":{}}}\n"
		write(t, legacyUser(home), body)
		out := removeVSCode(t)
		if !strings.Contains(out, "remove its dbgorilla entry by hand") {
			t.Errorf("no hand-edit hint:\n%s", out)
		}
		if data, _ := os.ReadFile(legacyUser(home)); string(data) != body {
			t.Error("commented file was rewritten")
		}
	})

	t.Run("--remove says nothing about old files that never held our entry", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		t.Chdir(t.TempDir())
		write(t, legacyUser(home), "// mine\n{\"servers\":{}}\n")
		if out := removeVSCode(t); strings.Contains(out, "by hand") || strings.Contains(out, "Could not clean") {
			t.Errorf("spurious warning:\n%s", out)
		}
	})
}
