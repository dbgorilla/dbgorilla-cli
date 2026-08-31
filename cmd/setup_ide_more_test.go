package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/spf13/cobra"
)

func setupIDETestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	c.Flags().StringSlice("client", nil, "")
	c.Flags().String("scope", "", "")
	c.Flags().Bool("list-clients", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Bool("print-config", false, "")
	c.Flags().Bool("print-key", false, "")
	c.Flags().Bool("print-admin-allowlist", false, "")
	c.Flags().Bool("no-claude-cli", false, "")
	c.Flags().Bool("rotate-key", false, "")
	c.Flags().Bool("remove", false, "")
	return c
}

func TestParseScope(t *testing.T) {
	cases := []struct {
		in      string
		want    ide.Scope
		wantErr bool
	}{
		{"", "", false},
		{"user", ide.ScopeUser, false},
		{"PROJECT", ide.ScopeProject, false},
		{" User ", ide.ScopeUser, false},
		{"global", "", true},
	}
	for _, tc := range cases {
		got, err := parseScope(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseScope(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("parseScope(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPickScope(t *testing.T) {
	w := &ide.Cursor{} // supports {user, project}, default user
	if got := pickScope(w, ""); got != ide.ScopeUser {
		t.Errorf("no override -> default, got %q", got)
	}
	if got := pickScope(w, ide.ScopeProject); got != ide.ScopeProject {
		t.Errorf("supported override should be honored, got %q", got)
	}
	if got := pickScope(w, ide.Scope("bogus")); got != ide.ScopeUser {
		t.Errorf("unsupported override should fall back to default, got %q", got)
	}
}

func TestEmitPrintConfig(t *testing.T) {
	out := capture(t, func() {
		if err := emitPrintConfig(&ide.Cursor{}, "https://dep/mcp/", "key-1"); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "mcpServers") || !strings.Contains(out, "https://dep/mcp/") {
		t.Errorf("print-config output wrong:\n%s", out)
	}
	if !strings.Contains(out, "Bearer key-1") {
		t.Errorf("entry should carry the bearer header:\n%s", out)
	}
}

func TestResolveSelectedAdapters(t *testing.T) {
	t.Run("empty flag uses detection", func(t *testing.T) {
		stubDetect(t, fakeWriter{name: "Fake", slug: "fake"})
		got, err := resolveSelectedAdapters(nil)
		if err != nil || len(got) != 1 || got[0].Slug() != "fake" {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})

	t.Run("explicit slugs resolve, blanks skipped", func(t *testing.T) {
		got, err := resolveSelectedAdapters([]string{"cursor", "", "vscode"})
		if err != nil || len(got) != 2 {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})

	t.Run("unknown slug errors", func(t *testing.T) {
		_, err := resolveSelectedAdapters([]string{"cursor", "nope"})
		if err == nil || !strings.Contains(err.Error(), "unknown client(s): nope") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPrintAdminAllowlist(t *testing.T) {
	out := capture(t, func() { printAdminAllowlist("https://dbg.acme.com/") })
	if !strings.Contains(out, "https://dbg.acme.com/mcp/") {
		t.Errorf("mcp URL wrong:\n%s", out)
	}
	if !strings.Contains(out, "Server name:  dbgorilla") {
		t.Errorf("server name missing:\n%s", out)
	}
}

func TestPrintClientList(t *testing.T) {
	isolate(t) // empty PATH -> deterministic (all undetected)
	out := capture(t, printClientList)
	for _, slug := range []string{"claude-code", "cursor", "vscode", "gemini", "opencode"} {
		if !strings.Contains(out, slug) {
			t.Errorf("client list missing %q:\n%s", slug, out)
		}
	}
}

func TestClaudeMCPAdd(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		stubExec(t, "", 0)
		if err := claudeMCPAdd("https://dep/mcp/", "key", "user"); err != nil {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("failure surfaces (redacted) output", func(t *testing.T) {
		stubExec(t, "boom", 1)
		err := claudeMCPAdd("https://dep/mcp/", "key", "user")
		if err == nil || !strings.Contains(err.Error(), "claude mcp add") {
			t.Fatalf("err=%v", err)
		}
	})
}

// --- runSetupIDE end-to-end variants --------------------------------------

func TestRunSetupIDE_ListClients(t *testing.T) {
	isolate(t)
	c := setupIDETestCmd()
	_ = c.Flags().Set("list-clients", "true")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "Supported MCP clients") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_PrintAdminAllowlist(t *testing.T) {
	isolate(t)
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", "https://dep.example")
	_ = c.Flags().Set("print-admin-allowlist", "true")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "https://dep.example/mcp/") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_PrintKey(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("client", "cursor")
	_ = c.Flags().Set("print-key", "true")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if strings.TrimSpace(out) != "mcp-key-xyz" {
		t.Errorf("print-key should emit just the key, got %q", out)
	}
}

func TestRunSetupIDE_PrintConfig(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("client", "cursor")
	_ = c.Flags().Set("print-config", "true")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "mcpServers") || !strings.Contains(out, "/mcp/") {
		t.Errorf("print-config output wrong:\n%s", out)
	}
}

func TestRunSetupIDE_DryRunWriter(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("client", "cursor")
	_ = c.Flags().Set("dry-run", "true")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "Would write MCP entry to") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_WritesCursorConfig(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("client", "cursor")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "Created") {
		t.Errorf("expected a freshly created config:\n%s", out)
	}
	if !strings.Contains(out, "Configured: 1") {
		t.Errorf("summary wrong:\n%s", out)
	}
}

func TestRunSetupIDE_InsecureTLSWarning(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "insecure", "true")
	_ = c.Flags().Set("client", "cursor")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "NODE_EXTRA_CA_CERTS") {
		t.Errorf("insecure deployments should warn about the CA cert:\n%s", out)
	}
}

func TestRunSetupIDE_HinterOnlyClient(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("client", "claude-desktop")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "manual setup") {
		t.Errorf("hint-only client should print manual instructions:\n%s", out)
	}
	if !strings.Contains(out, "Hinted: 1") {
		t.Errorf("summary should count the hint:\n%s", out)
	}
}

func TestRunSetupIDE_NoClientsDetected(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubDetect(t) // nothing installed
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", "https://dep.example")
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "No supported clients detected") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_UnknownClient(t *testing.T) {
	isolate(t)
	writeTokens(t)
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", "https://dep.example")
	_ = c.Flags().Set("client", "bogus")
	err := runSetupIDE(c, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown client(s): bogus") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSetupIDE_ClaudeCodeViaCLI(t *testing.T) {
	t.Run("dry-run prints the command", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		_ = c.Flags().Set("dry-run", "true")
		out := capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Would run: claude mcp add") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("registers via claude mcp add", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		stubExec(t, "", 0)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		out := capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Registered via `claude mcp add`") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("claude mcp add failure is reported", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		stubExec(t, "some failure", 1)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		var err error
		out := capture(t, func() { err = runSetupIDE(c, nil) })
		if err == nil || !strings.Contains(err.Error(), "failed to configure") {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(out, "Failed: 1") {
			t.Errorf("summary should count the failure:\n%s", out)
		}
	})

	t.Run("falls back to file write when claude CLI absent", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		stubLookPath(t, false)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		out := capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "falling back to direct config-file write") {
			t.Errorf("out=%q", out)
		}
		if !strings.Contains(out, "Created") {
			t.Errorf("fallback should write the config file:\n%s", out)
		}
	})
}

// --- the DBGorilla skill --------------------------------------------------

func TestRunSetupIDE_InstallsTheSkillForClaudeCode(t *testing.T) {
	skillPath := func(home string) string {
		return filepath.Join(home, ".claude", "skills", "dbgorilla", "SKILL.md")
	}

	t.Run("via claude mcp add", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		stubExec(t, "", 0)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		out := capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Installed the DBGorilla skill") {
			t.Errorf("out=%q", out)
		}
		if _, err := os.Stat(skillPath(home)); err != nil {
			t.Errorf("skill not on disk: %v", err)
		}
	})

	t.Run("also on the config-file fallback", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		stubLookPath(t, false) // no `claude` binary -> direct write
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if _, err := os.Stat(skillPath(home)); err != nil {
			t.Errorf("skill not on disk: %v", err)
		}
	})

	t.Run("re-running says so instead of churning the file", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		stubExec(t, "", 0)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		run := func() string {
			c := setupIDETestCmd()
			mustSet(t, c, "api-url", srv.URL)
			_ = c.Flags().Set("client", "claude-code")
			return capture(t, func() {
				if err := runSetupIDE(c, nil); err != nil {
					t.Fatalf("err=%v", err)
				}
			})
		}
		run()
		if out := run(); !strings.Contains(out, "Skill up to date") {
			t.Errorf("second run should report a no-op:\n%s", out)
		}
	})

	t.Run("no skill for clients that have no such thing", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "cursor")
		capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if _, err := os.Stat(skillPath(home)); !os.IsNotExist(err) {
			t.Errorf("configuring another client installed a Claude skill (err=%v)", err)
		}
	})

	t.Run("dry-run writes nothing", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		stubLookPath(t, true)
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "claude-code")
		_ = c.Flags().Set("dry-run", "true")
		out := capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Would install the DBGorilla skill") {
			t.Errorf("out=%q", out)
		}
		if _, err := os.Stat(skillPath(home)); !os.IsNotExist(err) {
			t.Errorf("dry-run wrote the skill (err=%v)", err)
		}
	})
}

// --- setup-ide --remove ---------------------------------------------------

func TestRunSetupIDE_Remove(t *testing.T) {
	// configure runs a real setup-ide first, so removal is tested against
	// what the command actually writes rather than against a fixture.
	configure := func(t *testing.T, client string) {
		t.Helper()
		srv := statusServer(t, 200, `"mcp-key-xyz"`)
		defer srv.Close()
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", client)
		capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("setup: %v", err)
			}
		})
	}
	remove := func(t *testing.T, client string, extra ...string) string {
		t.Helper()
		c := setupIDETestCmd()
		_ = c.Flags().Set("client", client)
		_ = c.Flags().Set("remove", "true")
		for _, f := range extra {
			_ = c.Flags().Set(f, "true")
		}
		return capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("remove: %v", err)
			}
		})
	}

	t.Run("takes the entry back out", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		configure(t, "cursor")
		path, err := (&ide.Cursor{}).ConfigPath(ide.ScopeUser)
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(before), "dbgorilla") {
			t.Fatalf("setup did not write the entry: %v", err)
		}
		out := remove(t, "cursor")
		if !strings.Contains(out, "Removed the dbgorilla entry") {
			t.Errorf("out=%q", out)
		}
		after, _ := os.ReadFile(path)
		if strings.Contains(string(after), "dbgorilla") {
			t.Errorf("entry survived:\n%s", after)
		}
	})

	t.Run("takes the skill with it for Claude Code", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		stubLookPath(t, false) // no `claude` binary; exercise the file path
		configure(t, "claude-code")
		skill := filepath.Join(home, ".claude", "skills", "dbgorilla", "SKILL.md")
		if _, err := os.Stat(skill); err != nil {
			t.Fatalf("setup did not install the skill: %v", err)
		}
		out := remove(t, "claude-code")
		if !strings.Contains(out, "Removed the DBGorilla skill") {
			t.Errorf("out=%q", out)
		}
		if _, err := os.Stat(skill); !os.IsNotExist(err) {
			t.Errorf("skill survived (err=%v)", err)
		}
	})

	t.Run("works without a session or a reachable deployment", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		configure(t, "cursor")
		if err := auth.ClearTokens(); err != nil {
			t.Fatal(err)
		}
		// No api-url set and no token: removal is local work and must not ask
		// for either. Anything that reached out would fail here.
		out := remove(t, "cursor")
		if !strings.Contains(out, "Removed the dbgorilla entry") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("removing twice says so rather than failing", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		configure(t, "cursor")
		remove(t, "cursor")
		if out := remove(t, "cursor"); !strings.Contains(out, "Nothing to remove") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("dry-run changes nothing", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		configure(t, "cursor")
		path, _ := (&ide.Cursor{}).ConfigPath(ide.ScopeUser)
		before, _ := os.ReadFile(path)
		out := remove(t, "cursor", "dry-run")
		if !strings.Contains(out, "Would remove the dbgorilla entry") {
			t.Errorf("out=%q", out)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Error("dry-run modified the config")
		}
	})

	t.Run("says the key is still live", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		configure(t, "cursor")
		// Removing an editor entry must not imply the credential is gone --
		// it is shared with every other editor and outlives this command.
		if out := remove(t, "cursor"); !strings.Contains(out, "logout") {
			t.Errorf("removal should point at logout for the key:\n%s", out)
		}
	})
}
