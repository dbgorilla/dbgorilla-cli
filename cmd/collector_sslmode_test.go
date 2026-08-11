package cmd

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/preflight"
	"github.com/spf13/cobra"
)

// TestMain keeps the TLS capability probe off the network by default. The
// probe dials a real server, so leaving it live made every install test wait
// out a connection timeout. Tests that care about probing call stubProbe.
func TestMain(m *testing.M) {
	probeTLS = func(context.Context, string) preflight.TLSSupport { return preflight.TLSUnknown }
	os.Exit(m.Run())
}

// stubProbe swaps the TLS capability probe for a fixed answer.
func stubProbe(t *testing.T, support preflight.TLSSupport) {
	t.Helper()
	orig := probeTLS
	probeTLS = func(context.Context, string) preflight.TLSSupport { return support }
	t.Cleanup(func() { probeTLS = orig })
}

func sslCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := baseCmd()
	c.Flags().String("ssl-mode", "verify-full", "")
	c.Flags().Bool("yes", false, "")
	return c
}

func TestResolveTLSMode(t *testing.T) {
	local := func() *collector.Target {
		return &collector.Target{Name: "t", Host: "localhost", Port: 5432, User: "ro"}
	}
	remote := func() *collector.Target {
		return &collector.Target{Name: "t", Host: "db.example.com", Port: 5432, User: "ro"}
	}

	t.Run("server speaks TLS: secure default is kept", func(t *testing.T) {
		stubProbe(t, preflight.TLSSupported)
		tgt := local()
		if err := resolveTLSMode(sslCmd(t), tgt, "pw"); err != nil {
			t.Fatalf("err=%v", err)
		}
		if tgt.SSLMode != "" {
			t.Errorf("SSLMode = %q, want untouched (flag default applies)", tgt.SSLMode)
		}
	})

	t.Run("undeterminable: nothing is inferred", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnknown)
		tgt := remote()
		if err := resolveTLSMode(sslCmd(t), tgt, "pw"); err != nil {
			t.Fatalf("err=%v", err)
		}
		if tgt.SSLMode != "" {
			t.Errorf("SSLMode = %q, want untouched when the probe cannot tell", tgt.SSLMode)
		}
	})

	t.Run("loopback without TLS: resolved automatically", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnsupported)
		for _, host := range []string{"localhost", "127.0.0.1", "::1", collector.DockerHostInternal} {
			tgt := local()
			tgt.Host = host
			out := capture(t, func() {
				if err := resolveTLSMode(sslCmd(t), tgt, "pw"); err != nil {
					t.Fatalf("host %s: err=%v", host, err)
				}
			})
			if tgt.SSLMode != "disable" {
				t.Errorf("host %s: SSLMode = %q, want disable", host, tgt.SSLMode)
			}
			if !strings.Contains(out, "on this machine") {
				t.Errorf("host %s: should say why this is safe, got %q", host, out)
			}
		}
	})

	// The hard constraint: a remote database that refuses TLS must never be
	// downgraded without the user saying so. Unattended runs refuse outright.
	t.Run("remote without TLS is never downgraded unattended", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnsupported)
		c := sslCmd(t)
		mustSet(t, c, "yes", "true") // --yes must NOT authorize a TLS downgrade
		tgt := remote()
		var err error
		out := capture(t, func() { err = resolveTLSMode(c, tgt, "pw") })
		if err == nil {
			t.Fatal("expected a refusal for a remote server without TLS")
		}
		if tgt.SSLMode == "disable" {
			t.Error("must not silently downgrade a remote connection")
		}
		if !strings.Contains(err.Error(), "--ssl-mode disable") {
			t.Errorf("error should name the explicit opt-in, got: %v", err)
		}
		if !strings.Contains(out, "clear text") {
			t.Errorf("should state the exposure in plain words, got %q", out)
		}
	})

	t.Run("explicit --ssl-mode is honored and skips probing", func(t *testing.T) {
		// A probe answer that would otherwise force a refusal.
		stubProbe(t, preflight.TLSUnsupported)
		c := sslCmd(t)
		mustSet(t, c, "ssl-mode", "require")
		tgt := remote()
		if err := resolveTLSMode(c, tgt, "pw"); err != nil {
			t.Fatalf("an explicit choice must be honored, got %v", err)
		}
		if tgt.SSLMode == "disable" {
			t.Error("explicit mode must not be overwritten")
		}
	})
}
