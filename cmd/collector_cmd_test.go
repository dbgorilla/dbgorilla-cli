package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// statusServer returns an httptest server that replies with the same status
// code + body to every request. Handy for single-endpoint command tests.
func statusServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
}

func installTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	c.Flags().String("name", "", "")
	c.Flags().String("db-host", "localhost", "")
	c.Flags().Int("db-port", 5432, "")
	c.Flags().String("db-name", "", "")
	c.Flags().String("db-user", "", "")
	c.Flags().String("db-password", "", "")
	c.Flags().String("ssl-mode", "verify-full", "")
	c.Flags().String("image", collector.DefaultImage, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().String("keycloak-url", "", "")
	c.Flags().String("otlp-url", "", "")
	c.Flags().String("opamp-url", "", "")
	c.Flags().String("ca-cert", "", "")
	c.Flags().Bool("force", false, "")
	return c
}

func uninstallTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	c.Flags().Bool("yes", false, "")
	return c
}

func TestRunInstall_DryRun(t *testing.T) {
	isolate(t)
	c := installTestCmd()
	_ = c.Flags().Set("dry-run", "true")
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", "localhost")
	out := capture(t, func() {
		if err := runInstall(c, nil); err != nil {
			t.Fatalf("dry-run err: %v", err)
		}
	})
	for _, want := range []string{
		"DRY RUN", "Would write config", "docker run",
		"--- collector.toml ---", "Would rewrite localhost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestRunInstall_DryRun_RemoteHostNoRewrite(t *testing.T) {
	isolate(t)
	c := installTestCmd()
	_ = c.Flags().Set("dry-run", "true")
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", "db.remote.example")
	out := capture(t, func() {
		if err := runInstall(c, nil); err != nil {
			t.Fatalf("dry-run err: %v", err)
		}
	})
	if strings.Contains(out, "Would rewrite") {
		t.Errorf("remote host should not be rewritten:\n%s", out)
	}
}

func TestRunInstall_EarlyErrors(t *testing.T) {
	t.Run("no api url", func(t *testing.T) {
		isolate(t)
		c := installTestCmd()
		_ = c.Flags().Set("db-user", "ro")
		err := runInstall(c, nil)
		if err == nil || !strings.Contains(err.Error(), "API URL") {
			t.Fatalf("err=%v, want api-url error", err)
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		isolate(t)
		c := installTestCmd()
		_ = c.Flags().Set("api-url", "https://api.example")
		_ = c.Flags().Set("db-user", "ro")
		err := runInstall(c, nil)
		if err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("err=%v, want login error", err)
		}
	})

	t.Run("refuses to clobber an existing install", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		if err := collector.SaveState(&collector.State{AgentID: "existing-agent"}); err != nil {
			t.Fatal(err)
		}
		c := installTestCmd()
		_ = c.Flags().Set("api-url", "https://api.example")
		_ = c.Flags().Set("db-user", "ro")
		err := runInstall(c, nil)
		if err == nil || !strings.Contains(err.Error(), "already installed") {
			t.Fatalf("err=%v, want already-installed error", err)
		}
		if !strings.Contains(err.Error(), "existing-agent") {
			t.Errorf("error should name the existing agent: %v", err)
		}
	})
}

func TestRunStatus(t *testing.T) {
	t.Run("no collector installed", func(t *testing.T) {
		isolate(t)
		out := capture(t, func() {
			if err := runStatus(baseCmd(), nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "No collector installed") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("installed but container missing, unauthenticated", func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{
			AgentID: "a1", TenantID: "t1", TargetName: "db", Image: "img", ContainerName: "dbg-collector",
		}); err != nil {
			t.Fatal(err)
		}
		out := capture(t, func() {
			if err := runStatus(baseCmd(), nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Agent:") || !strings.Contains(out, "a1") {
			t.Errorf("status header missing:\n%s", out)
		}
		if !strings.Contains(out, "Container:  missing") {
			t.Errorf("want container missing (docker unavailable):\n%s", out)
		}
	})

	t.Run("installed + authenticated shows live connection", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `{"status":"connected"}`)
		defer srv.Close()
		if err := collector.SaveState(&collector.State{AgentID: "a1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runStatus(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Connection: connected") {
			t.Errorf("want live connection status:\n%s", out)
		}
	})
}

func TestRunList(t *testing.T) {
	t.Run("no api url", func(t *testing.T) {
		isolate(t)
		if err := runList(baseCmd(), nil); err == nil {
			t.Fatal("want api-url error")
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		mustSet(t, c, "api-url", "https://api.example")
		if err := runList(c, nil); err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("empty tenant list", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 200, `{"items":[]}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runList(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "No collectors registered") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("lists collectors and flags the local machine", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		if err := collector.SaveState(&collector.State{AgentID: "agent-1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		srv := statusServer(t, 200,
			`{"items":[{"agent_id":"agent-1","status":"connected","name":"laptop"},{"agent_id":"agent-2","status":"offline"}]}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runList(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "agent-1") || !strings.Contains(out, "agent-2") {
			t.Errorf("both agents should appear:\n%s", out)
		}
		if !strings.Contains(out, "(this machine)") {
			t.Errorf("local agent should be marked:\n%s", out)
		}
	})

	t.Run("backend error propagates", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 500, `{"detail":"boom"}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		if err := runList(c, nil); err == nil {
			t.Fatal("want backend error")
		}
	})
}

func TestRunUninstall(t *testing.T) {
	t.Run("nothing installed", func(t *testing.T) {
		isolate(t)
		out := capture(t, func() {
			if err := runUninstall(uninstallTestCmd(), nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "No collector installed") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("declining the confirmation aborts", func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{AgentID: "a1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		setStdin(t, "n\n")
		var err error
		_ = capture(t, func() { err = runUninstall(uninstallTestCmd(), nil) })
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("err=%v, want aborted", err)
		}
	})

	t.Run("keeps state when not logged in", func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{AgentID: "a1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		c := uninstallTestCmd()
		_ = c.Flags().Set("yes", "true")
		out := capture(t, func() {
			if err := runUninstall(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "not logged in") {
			t.Errorf("want not-logged-in notice:\n%s", out)
		}
		if st, _ := collector.LoadState(); st == nil {
			t.Error("state must be preserved so the user can retry after login")
		}
	})

	t.Run("deprovisions and clears local state when authenticated", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 204, "")
		defer srv.Close()
		if err := collector.SaveState(&collector.State{AgentID: "a1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		c := uninstallTestCmd()
		_ = c.Flags().Set("yes", "true")
		mustSet(t, c, "api-url", srv.URL)
		out := capture(t, func() {
			if err := runUninstall(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Identity deprovisioned") || !strings.Contains(out, "Local config and secrets cleared") {
			t.Errorf("want full teardown:\n%s", out)
		}
		if st, _ := collector.LoadState(); st != nil {
			t.Error("state should be removed after successful deprovision")
		}
	})
}
