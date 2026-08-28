package cmd

import (
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/spf13/cobra"
)

func loginTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	c.Flags().String("mode", "", "")
	c.Flags().String("tenant", "", "")
	c.Flags().String("account", "", "")
	c.Flags().Bool("verbose", false, "")
	return c
}

const deviceConfigPath = "/api/v0_1/auth/keycloak/device-config"
const tokenPath = "/api/v0_1/auth/token"

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("all-empty should be empty, got %q", got)
	}
	if got := firstNonEmpty("first", "second"); got != "first" {
		t.Errorf("got %q", got)
	}
}

func TestRunLogin_UnknownMode(t *testing.T) {
	isolate(t)
	c := loginTestCmd()
	mustSet(t, c, "api-url", "https://dep.example")
	_ = c.Flags().Set("mode", "bogus")
	err := runLogin(c, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("err=%v", err)
	}
}

// Login with nothing configured no longer errors -- it resolves the built-in
// production default and proceeds. That resolution is covered by
// TestRequireAPIURL and the config package tests; exercising it here would
// hit the real production endpoint.

func TestRunLogin_SSOModeDeviceConfigUnavailable(t *testing.T) {
	isolate(t)
	srv := statusServer(t, 404, "") // device-config not exposed
	defer srv.Close()
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("mode", "sso")
	var err error
	_ = capture(t, func() { err = runLogin(c, nil) })
	if err == nil {
		t.Fatal("expected device flow to fail when device-config is 404")
	}
}

func TestRunLogin_AutoDetectFallsBackToPassword(t *testing.T) {
	isolate(t)
	srv := routingServer(t, map[string]resp{
		deviceConfigPath: {404, ""}, // no SSO -> auto-detect picks password
		tokenPath:        {200, `{"access_token":"tok","refresh_token":"r","expires_in":3600}`},
		authPath: {200,
			`{"email":"dev@acme.com","organization":"Acme","id":"u-1","tenant_id":"t-9","role":"admin"}`},
	})
	defer srv.Close()
	setStdin(t, "s3cret\n") // password prompt (tenant/account come from flags)
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("tenant", "acme")
	_ = c.Flags().Set("account", "dev")
	var err error
	out := capture(t, func() { err = runLogin(c, nil) })
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "Signed in as dev@acme.com  (org: Acme)") {
		t.Errorf("out=%q", out)
	}
	// The organization's id is not what a person came to read, so the
	// success line must not carry it.
	if strings.Contains(out, "t-9") {
		t.Errorf("sign-in line leaked the organization id: %q", out)
	}
	// Login should persist the resolved api-url for subsequent commands.
	if cfg, _ := config.LoadUser(); cfg.API.URL != srv.URL {
		t.Errorf("api-url not persisted, cfg=%+v", cfg)
	}
}

func TestRunLogin_PasswordAuthFailure(t *testing.T) {
	isolate(t)
	srv := routingServer(t, map[string]resp{
		tokenPath: {401, ""},
	})
	defer srv.Close()
	setStdin(t, "wrongpw\n")
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("mode", "password")
	_ = c.Flags().Set("tenant", "acme")
	_ = c.Flags().Set("account", "dev")
	var err error
	_ = capture(t, func() { err = runLogin(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunLogin_WarnsOnPersistedInsecure(t *testing.T) {
	isolate(t)
	cfg := &config.Config{}
	cfg.API.Insecure = true
	if err := cfg.SaveUser(); err != nil {
		t.Fatal(err)
	}
	srv := statusServer(t, 404, "")
	defer srv.Close()
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("mode", "sso") // will fail after the warning; we only assert the warning
	out := capture(t, func() { _ = runLogin(c, nil) })
	if !strings.Contains(out, "persisted `insecure = true`") {
		t.Errorf("expected persisted-insecure warning:\n%s", out)
	}
}

func TestPersistLoginState(t *testing.T) {
	t.Run("saves api-url on fresh config", func(t *testing.T) {
		isolate(t)
		out := capture(t, func() { persistLoginState("https://dep.example", false, false) })
		if !strings.Contains(out, "Saved api-url to") {
			t.Errorf("out=%q", out)
		}
		if cfg, _ := config.LoadUser(); cfg.API.URL != "https://dep.example" {
			t.Errorf("cfg=%+v", cfg)
		}
	})

	t.Run("saves insecure when flag explicitly set", func(t *testing.T) {
		isolate(t)
		out := capture(t, func() { persistLoginState("https://dep.example", true, true) })
		if !strings.Contains(out, "insecure=true") {
			t.Errorf("out=%q", out)
		}
		if cfg, _ := config.LoadUser(); !cfg.API.Insecure {
			t.Errorf("insecure not persisted, cfg=%+v", cfg)
		}
	})

	t.Run("no write when nothing changed", func(t *testing.T) {
		isolate(t)
		cfg := &config.Config{}
		cfg.API.URL = "https://dep.example"
		if err := cfg.SaveUser(); err != nil {
			t.Fatal(err)
		}
		out := capture(t, func() { persistLoginState("https://dep.example", false, false) })
		if strings.Contains(out, "Saved") {
			t.Errorf("should not re-save unchanged config:\n%s", out)
		}
	})
}
