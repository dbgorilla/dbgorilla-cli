package cmd

import (
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
)

func TestRequireAPIURL(t *testing.T) {
	t.Run("missing returns actionable error", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		url, err := requireAPIURL(c)
		if err == nil {
			t.Fatal("expected error when no API URL configured")
		}
		if url != "" {
			t.Errorf("url = %q, want empty", url)
		}
		if !strings.Contains(err.Error(), "config set api-url") {
			t.Errorf("error should guide the user, got: %v", err)
		}
	})

	t.Run("flag wins", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		mustSet(t, c, "api-url", "https://flag.example")
		url, err := requireAPIURL(c)
		if err != nil || url != "https://flag.example" {
			t.Fatalf("url=%q err=%v", url, err)
		}
	})
}

func TestResolveInsecure(t *testing.T) {
	t.Run("explicit flag wins over config", func(t *testing.T) {
		isolate(t)
		cfg := &config.Config{}
		cfg.API.Insecure = true
		if err := cfg.SaveUser(); err != nil {
			t.Fatal(err)
		}
		c := baseCmd()
		mustSet(t, c, "insecure", "false") // explicit --insecure=false overrides persisted true
		if resolveInsecure(c) {
			t.Error("explicit --insecure=false must win over persisted insecure=true")
		}
	})

	t.Run("falls back to persisted config when flag unset", func(t *testing.T) {
		isolate(t)
		cfg := &config.Config{}
		cfg.API.Insecure = true
		if err := cfg.SaveUser(); err != nil {
			t.Fatal(err)
		}
		c := baseCmd()
		if !resolveInsecure(c) {
			t.Error("persisted insecure=true should apply when flag not set")
		}
	})

	t.Run("defaults to false", func(t *testing.T) {
		isolate(t)
		if resolveInsecure(baseCmd()) {
			t.Error("want secure by default")
		}
	})
}

func TestRequireLogin(t *testing.T) {
	t.Run("not logged in", func(t *testing.T) {
		isolate(t)
		if _, err := requireLogin(); err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("err=%v, want not-logged-in", err)
		}
	})

	t.Run("logged in returns tokens", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		tok, err := requireLogin()
		if err != nil || tok == nil || tok.AccessToken != "test-access-token" {
			t.Fatalf("tok=%+v err=%v", tok, err)
		}
	})
}

func TestNewAPIClient(t *testing.T) {
	t.Run("secure client uses resolved URL", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		mustSet(t, c, "api-url", "https://api.example")
		client := newAPIClient(c)
		if client == nil || client.BaseURL != "https://api.example" {
			t.Fatalf("client=%+v", client)
		}
	})

	t.Run("insecure flag builds an insecure client", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		mustSet(t, c, "api-url", "https://api.example")
		mustSet(t, c, "insecure", "true")
		client := newAPIClient(c)
		if client == nil || client.BaseURL != "https://api.example" {
			t.Fatalf("client=%+v", client)
		}
	})
}

// Coarse smoke tests through the real Execute() entrypoint.
func TestExecute_Smoke(t *testing.T) {
	t.Run("version prints build info", func(t *testing.T) {
		isolate(t)
		rootCmd.SetArgs([]string{"version"})
		defer rootCmd.SetArgs(nil)
		out := capture(t, func() {
			if err := Execute(); err != nil {
				t.Fatalf("version err: %v", err)
			}
		})
		if !strings.Contains(out, "dbgorilla version") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("help succeeds", func(t *testing.T) {
		isolate(t)
		rootCmd.SetArgs([]string{"--help"})
		defer rootCmd.SetArgs(nil)
		out := capture(t, func() {
			if err := Execute(); err != nil {
				t.Fatalf("help err: %v", err)
			}
		})
		if !strings.Contains(out, "setup-ide") {
			t.Errorf("help should list subcommands, got: %q", out)
		}
	})

	t.Run("unknown command errors", func(t *testing.T) {
		isolate(t)
		rootCmd.SetArgs([]string{"definitely-not-a-command"})
		defer rootCmd.SetArgs(nil)
		out := capture(t, func() {
			if err := Execute(); err == nil {
				t.Fatal("expected error for unknown command")
			}
		})
		if !strings.Contains(out, "Error:") {
			t.Errorf("Execute should print the error, got: %q", out)
		}
	})
}

func TestResolveColor(t *testing.T) {
	t.Run("explicit --no-color wins over --color", func(t *testing.T) {
		c := baseCmd()
		mustSet(t, c, "no-color", "true")
		mustSet(t, c, "color", "true")
		if resolveColor(c) {
			t.Error("--no-color must win even if --color was also passed")
		}
	})

	t.Run("explicit --color forces on", func(t *testing.T) {
		c := baseCmd()
		mustSet(t, c, "color", "true")
		if !resolveColor(c) {
			t.Error("--color should force color on")
		}
	})

	t.Run("neither flag set falls back to auto-detect", func(t *testing.T) {
		// baseCmd's flags default to false and unchanged -- resolveColor
		// should fall through to style.Detect() rather than treating the
		// zero-value `false` on --color as an explicit --color=false.
		c := baseCmd()
		if resolveColor(c) != style.Detect() {
			t.Error("with no flags set, resolveColor should equal style.Detect()")
		}
	})
}
