package cmd

import (
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/spf13/cobra"
)

func whoamiTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	c.Flags().Bool("json", false, "")
	return c
}

func TestRunWhoami(t *testing.T) {
	t.Run("no api url", func(t *testing.T) {
		isolate(t)
		if err := runWhoami(whoamiTestCmd(), nil); err == nil {
			t.Fatal("want api-url error")
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		isolate(t)
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", "https://dep.example")
		if err := runWhoami(c, nil); err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("err=%v", err)
		}
	})

	// `whoami` answers an identity question, and the answer usually gets
	// pasted somewhere. The organization gets named on the top line, because
	// the name is what a person recognises; the ids follow underneath,
	// because the ids are what actually identifies the account to support.
	// Both, unprompted -- no flag to discover.
	t.Run("identity leads with the org name and then gives the ids", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200,
			`{"email":"dev@acme.com","organization":"Acme","id":"u-1","tenant_id":"t-9","role":"admin"}`)
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runWhoami(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "dev@acme.com  (org: Acme)") {
			t.Errorf("out=%q", out)
		}
		for _, want := range []string{"role:", "admin", "user-id:", "u-1", "org-id:", "t-9"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in %q", want, out)
			}
		}
		// The name has to come before the ids, or the line a person reads is
		// buried under the lines they do not.
		if strings.Index(out, "(org: Acme)") > strings.Index(out, "org-id:") {
			t.Errorf("ids printed before the name: %q", out)
		}
	})

	t.Run("identity without an org name falls back to the id", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		// organization empty, tenant_id populated -> falls back to tenant_id,
		// because the raw id still beats printing "(org: )".
		srv := statusServer(t, 200, `{"username":"dev","tenant_id":"t-9"}`)
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runWhoami(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "dev  (org: t-9)") {
			t.Errorf("out=%q", out)
		}
	})

	// Neither name nor id. The line says who you are and stops, rather than
	// printing an empty "(org: )" that reads like something failed.
	t.Run("identity with no organization at all omits the org entirely", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `{"username":"dev"}`)
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runWhoami(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if strings.Contains(out, "org") {
			t.Errorf("want no org fragment at all, got %q", out)
		}
		if !strings.Contains(out, "dev") {
			t.Errorf("want the account named, got %q", out)
		}
	})

	t.Run("json output", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `{"email":"dev@acme.com","organization":"Acme"}`)
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("json", "true")
		out := capture(t, func() {
			if err := runWhoami(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, `"email": "dev@acme.com"`) {
			t.Errorf("want indented JSON, got %q", out)
		}
	})

	t.Run("401 token expired", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 401, "")
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		if err := runWhoami(c, nil); err == nil || !strings.Contains(err.Error(), "token expired") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 503, "")
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		if err := runWhoami(c, nil); err == nil || !strings.Contains(err.Error(), "unexpected response") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `not json`)
		defer srv.Close()
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		if err := runWhoami(c, nil); err == nil || !strings.Contains(err.Error(), "cannot parse identity") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		c := whoamiTestCmd()
		mustSet(t, c, "api-url", "http://127.0.0.1:1")
		if err := runWhoami(c, nil); err == nil || !strings.Contains(err.Error(), "cannot reach API") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestConfigSet(t *testing.T) {
	t.Run("api-url", func(t *testing.T) {
		isolate(t)
		out := capture(t, func() {
			if err := configSetCmd.RunE(configSetCmd, []string{"api-url", "https://dep.example"}); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Set api-url = https://dep.example") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("insecure bool", func(t *testing.T) {
		isolate(t)
		if err := configSetCmd.RunE(configSetCmd, []string{"insecure", "true"}); err != nil {
			t.Fatalf("err=%v", err)
		}
		if cfg, _ := loadUserCfgInsecure(); !cfg {
			t.Error("insecure not saved")
		}
	})

	t.Run("insecure invalid bool errors", func(t *testing.T) {
		isolate(t)
		if err := configSetCmd.RunE(configSetCmd, []string{"insecure", "notabool"}); err == nil {
			t.Fatal("want invalid-boolean error")
		}
	})

	t.Run("unknown key errors", func(t *testing.T) {
		isolate(t)
		if err := configSetCmd.RunE(configSetCmd, []string{"nope", "x"}); err == nil {
			t.Fatal("want unknown-key error")
		}
	})
}

func TestConfigGet(t *testing.T) {
	t.Run("api-url from user config with source", func(t *testing.T) {
		isolate(t)
		if err := configSetCmd.RunE(configSetCmd, []string{"api-url", "https://dep.example"}); err != nil {
			t.Fatal(err)
		}
		c := baseCmd()
		out := capture(t, func() {
			if err := configGetCmd.RunE(c, []string{"api-url"}); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "https://dep.example") || !strings.Contains(out, "source: user-config") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("api-url not set falls back to default", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		out := capture(t, func() {
			if err := configGetCmd.RunE(c, []string{"api-url"}); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, config.DefaultAPIURL) || !strings.Contains(out, "source: default") {
			t.Errorf("out=%q, want the default URL with source: default", out)
		}
	})

	t.Run("insecure", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		out := capture(t, func() {
			if err := configGetCmd.RunE(c, []string{"insecure"}); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "insecure: false") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("unknown key errors", func(t *testing.T) {
		isolate(t)
		if err := configGetCmd.RunE(baseCmd(), []string{"nope"}); err == nil {
			t.Fatal("want unknown-key error")
		}
	})
}

func TestConfigUnset(t *testing.T) {
	t.Run("api-url", func(t *testing.T) {
		isolate(t)
		if err := configSetCmd.RunE(configSetCmd, []string{"api-url", "https://dep.example"}); err != nil {
			t.Fatal(err)
		}
		out := capture(t, func() {
			if err := configUnsetCmd.RunE(configUnsetCmd, []string{"api-url"}); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Unset api-url") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("insecure", func(t *testing.T) {
		isolate(t)
		if err := configUnsetCmd.RunE(configUnsetCmd, []string{"insecure"}); err != nil {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("unknown key errors", func(t *testing.T) {
		isolate(t)
		if err := configUnsetCmd.RunE(configUnsetCmd, []string{"nope"}); err == nil {
			t.Fatal("want unknown-key error")
		}
	})
}

// loadUserCfgInsecure is a tiny helper for asserting persisted insecure state.
func loadUserCfgInsecure() (bool, error) {
	// import cycle-free: use the same path resolution the code under test uses.
	c := baseCmd()
	return resolveInsecure(c), nil
}

func TestVersionCommand(t *testing.T) {
	out := capture(t, func() { versionCmd.Run(versionCmd, nil) })
	if !strings.Contains(out, "dbgorilla version") || !strings.Contains(out, "commit") {
		t.Errorf("out=%q", out)
	}
}

func TestResolveVersion(t *testing.T) {
	t.Run("released ldflag version is authoritative", func(t *testing.T) {
		old := Version
		Version = "9.9.9"
		defer func() { Version = old }()
		v, _, _ := resolveVersion()
		if v != "9.9.9" {
			t.Errorf("v=%q, want 9.9.9", v)
		}
	})

	t.Run("dev build resolves from build info", func(t *testing.T) {
		old := Version
		Version = "dev"
		defer func() { Version = old }()
		v, _, _ := resolveVersion()
		if v == "" {
			t.Error("version must never be empty")
		}
	})
}

func TestInstalledViaBrew(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/bin/dbg", true},
		{"/usr/local/Cellar/dbg/1.0/bin/dbg", true},
		{"/home/linuxbrew/.linuxbrew/bin/dbg", true},
		{"/usr/bin/dbg", false},
		{"/opt/homebrew", false}, // exactly the prefix (no suffix) -> not "under" it
	}
	for _, tc := range cases {
		stubExecutable(t, tc.path)
		if got := installedViaBrew(); got != tc.want {
			t.Errorf("installedViaBrew(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestRunUpgrade(t *testing.T) {
	t.Run("non-brew prints manual instructions", func(t *testing.T) {
		isolate(t)
		stubExecutable(t, "/usr/bin/dbg")
		out := capture(t, func() {
			if err := runUpgrade(nil, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Not a Homebrew install") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("brew install runs brew upgrade", func(t *testing.T) {
		isolate(t)
		stubExecutable(t, "/opt/homebrew/bin/dbg")
		stubExec(t, "", 0)
		out := capture(t, func() {
			if err := runUpgrade(nil, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Detected Homebrew install") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("brew install targets the dbgorilla formula, not dbg", func(t *testing.T) {
		// Pins the actual Homebrew formula name (`dbgorilla`, matching
		// `brew install dbgorilla/tap/dbgorilla`). A prior version of this
		// command ran `brew upgrade dbgorilla/tap/dbg`, which fails outright
		// -- there is no `dbg` formula, only a `dbg` symlink installed by
		// the `dbgorilla` formula.
		isolate(t)
		stubExecutable(t, "/opt/homebrew/bin/dbg")
		orig := execCommand
		var gotName string
		var gotArgs []string
		execCommand = func(name string, args ...string) *exec.Cmd {
			gotName, gotArgs = name, args
			return exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
		}
		t.Cleanup(func() { execCommand = orig })
		_ = capture(t, func() { _ = runUpgrade(nil, nil) })
		if gotName != "brew" {
			t.Errorf("command = %q, want brew", gotName)
		}
		if want := []string{"upgrade", "dbgorilla/tap/dbgorilla"}; !slices.Equal(gotArgs, want) {
			t.Errorf("args = %v, want %v", gotArgs, want)
		}
	})
}

func TestLogout(t *testing.T) {
	// runLogout drives logoutCmd against a stub deployment. The api-url flag
	// has to come from a command we build, because logoutCmd only inherits the
	// real root's persistent flags when it is executed through it -- and
	// without it, logout would resolve the built-in production URL and try to
	// revoke a key there.
	runLogout := func(t *testing.T, apiURL string) string {
		t.Helper()
		c := baseCmd()
		mustSet(t, c, "api-url", apiURL)
		return capture(t, func() {
			if err := logoutCmd.RunE(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}

	t.Run("clears the tokens and revokes the MCP key", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv, seen := keyServer(t, map[string]resp{http.MethodDelete: {200, ""}})

		out := runLogout(t, srv.URL)
		if !strings.Contains(out, "Signed out.") {
			t.Errorf("out=%q", out)
		}
		if tok, _ := auth.LoadTokens(); tok != nil {
			t.Error("tokens should be cleared after logout")
		}
		// Clearing the tokens alone used to leave the MCP key live, so an
		// editor configured with it kept working after sign-out.
		if len(*seen) != 1 || (*seen)[0] != http.MethodDelete {
			t.Errorf("want the key revoked, got methods=%v", *seen)
		}
	})

	t.Run("a deployment that cannot revoke does not block signing out", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv, _ := keyServer(t, map[string]resp{http.MethodDelete: {503, ""}})

		out := runLogout(t, srv.URL)
		if !strings.Contains(out, "Signed out.") {
			t.Errorf("sign-out must still complete:\n%s", out)
		}
		if tok, _ := auth.LoadTokens(); tok != nil {
			t.Error("tokens should be cleared even when the revoke fails")
		}
		// And it has to say so -- a key the user believes is dead is worse
		// than one they know is live.
		if !strings.Contains(out, "still valid") {
			t.Errorf("failure to revoke must be reported:\n%s", out)
		}
	})

	t.Run("an unreachable deployment does not block signing out", func(t *testing.T) {
		isolate(t)
		writeTokens(t)

		// Nothing listening: the revoke fails at the transport, before any
		// status code exists to interpret.
		out := runLogout(t, "http://127.0.0.1:1")
		if !strings.Contains(out, "Signed out.") {
			t.Errorf("sign-out must still complete:\n%s", out)
		}
		if tok, _ := auth.LoadTokens(); tok != nil {
			t.Error("tokens should be cleared even when the deployment is down")
		}
		if !strings.Contains(out, "still valid") {
			t.Errorf("failure to revoke must be reported:\n%s", out)
		}
	})

	t.Run("no session means nothing to revoke", func(t *testing.T) {
		isolate(t)
		srv, seen := keyServer(t, map[string]resp{})

		out := runLogout(t, srv.URL)
		if !strings.Contains(out, "Signed out.") {
			t.Errorf("out=%q", out)
		}
		if len(*seen) != 0 {
			t.Errorf("logged-out logout should call nothing, got methods=%v", *seen)
		}
	})
}
