package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
)

// After a successful login, a failing /auth/user must not fail the command --
// the tokens stored fine; identity is just cosmetic.
func TestRunLogin_SignedInButIdentityFetchFails(t *testing.T) {
	isolate(t)
	srv := routingServer(t, map[string]resp{
		tokenPath: {200, `{"access_token":"tok","expires_in":3600}`},
		authPath:  {500, ""}, // identity lookup fails post-login
	})
	defer srv.Close()
	setStdin(t, "pw\n")
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("mode", "password")
	_ = c.Flags().Set("tenant", "acme")
	_ = c.Flags().Set("account", "dev")
	var err error
	out := capture(t, func() { err = runLogin(c, nil) })
	if err != nil {
		t.Fatalf("login should still succeed, err=%v", err)
	}
	if !strings.Contains(out, "could not fetch identity") {
		t.Errorf("out=%q", out)
	}
}

// Exercises the merge-into-existing-file and idempotent-no-op branches of the
// setup-ide writer switch.
func TestRunSetupIDE_MergeThenNoop(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"mcp-key-xyz"`)
	defer srv.Close()

	// Pre-seed the Cursor user config with an unrelated server entry.
	cursorPath, err := (&ide.Cursor{}).ConfigPath(ide.ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers":{"other":{"url":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func() string {
		c := setupIDETestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("client", "cursor")
		return capture(t, func() {
			if err := runSetupIDE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}

	first := run()
	if !strings.Contains(first, "Merged dbgorilla entry") {
		t.Errorf("first run should merge into the existing file:\n%s", first)
	}
	second := run()
	if !strings.Contains(second, "Up to date") {
		t.Errorf("second run should be a no-op:\n%s", second)
	}
}
