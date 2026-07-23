package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/preflight"
	"github.com/spf13/cobra"
)

func TestWithDefaultPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"https://host", "https://host:443"},
		{"http://host", "http://host:80"},
		{"https://host:8443", "https://host:8443"}, // already has port -> untouched
		{"ftp://host", "ftp://host"},               // unknown scheme -> untouched
		{"://bad", "://bad"},                       // unparseable/no host -> untouched
	}
	for _, tc := range cases {
		if got := withDefaultPort(tc.in); got != tc.want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func endpointFlagCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("auth-url", "", "")
	c.Flags().String("keycloak-url", "", "")
	c.Flags().String("otlp-url", "", "")
	c.Flags().String("opamp-url", "", "")
	return c
}

func TestEndpointsFromFlags(t *testing.T) {
	c := endpointFlagCmd()
	_ = c.Flags().Set("auth-url", "https://auth")
	_ = c.Flags().Set("otlp-url", "https://otlp")
	e := endpointsFromFlags(c)
	if e.AuthBaseURL != "https://auth" || e.OtlpBaseURL != "https://otlp" || e.OpampBaseURL != "" {
		t.Errorf("endpointsFromFlags = %+v", e)
	}
}

func TestEndpointsFromFlags_DeprecatedKeycloakFlag(t *testing.T) {
	c := endpointFlagCmd()
	_ = c.Flags().Set("keycloak-url", "https://legacy-auth")
	if e := endpointsFromFlags(c); e.AuthBaseURL != "https://legacy-auth" {
		t.Errorf("deprecated --keycloak-url not honored: %+v", e)
	}
}

func TestEndpointsFor(t *testing.T) {
	t.Run("mint response is the base, flags override", func(t *testing.T) {
		c := endpointFlagCmd()
		_ = c.Flags().Set("otlp-url", "https://flag-otlp") // override just OTLP
		creds := &api.CollectorCredentials{
			AuthBaseURL:  "https://mint-auth",
			OtlpBaseURL:  "https://mint-otlp",
			OpampBaseURL: "https://mint-opamp",
		}
		e := endpointsFor(creds, c)
		if e.AuthBaseURL != "https://mint-auth" {
			t.Errorf("auth = %q, want mint value", e.AuthBaseURL)
		}
		if e.OpampBaseURL != "https://mint-opamp" {
			t.Errorf("opamp = %q, want mint value", e.OpampBaseURL)
		}
		// OTLP overridden by flag, then default-port applied.
		if e.OtlpBaseURL != "https://flag-otlp:443" {
			t.Errorf("otlp = %q, want flag value with default port", e.OtlpBaseURL)
		}
	})

	t.Run("bare OTLP host from mint gets default port", func(t *testing.T) {
		creds := &api.CollectorCredentials{OtlpBaseURL: "https://otlp.internal"}
		e := endpointsFor(creds, endpointFlagCmd())
		if e.OtlpBaseURL != "https://otlp.internal:443" {
			t.Errorf("otlp = %q", e.OtlpBaseURL)
		}
	})
}

func TestBuildDSN(t *testing.T) {
	t.Run("uses first configured database", func(t *testing.T) {
		got := buildDSN(collector.Target{
			Host: "db.local", Port: 5433, User: "ro", SSLMode: "require",
			Databases: []string{"app", "other"},
		}, "s3cr3t")
		if !strings.HasPrefix(got, "postgres://ro:s3cr3t@db.local:5433/app") {
			t.Errorf("dsn = %q", got)
		}
		if !strings.Contains(got, "sslmode=require") {
			t.Errorf("dsn missing sslmode: %q", got)
		}
	})

	t.Run("defaults to postgres db and omits empty sslmode", func(t *testing.T) {
		got := buildDSN(collector.Target{Host: "h", Port: 5432, User: "u"}, "p")
		if !strings.Contains(got, "/postgres") {
			t.Errorf("want /postgres default db, got %q", got)
		}
		if strings.Contains(got, "sslmode=") {
			t.Errorf("empty sslmode should be omitted, got %q", got)
		}
	})
}

func caCertCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("ca-cert", "", "")
	return c
}

func TestResolveCACert(t *testing.T) {
	t.Run("empty stays empty", func(t *testing.T) {
		got, err := resolveCACert(caCertCmd())
		if got != "" || err != nil {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("existing file resolves to absolute path", func(t *testing.T) {
		dir := t.TempDir()
		pem := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(pem, []byte("-----BEGIN CERTIFICATE-----\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c := caCertCmd()
		_ = c.Flags().Set("ca-cert", pem)
		got, err := resolveCACert(c)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("want absolute path, got %q", got)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		c := caCertCmd()
		_ = c.Flags().Set("ca-cert", filepath.Join(t.TempDir(), "nope.pem"))
		if _, err := resolveCACert(c); err == nil {
			t.Fatal("expected error for missing ca-cert")
		}
	})
}

func TestFirstString(t *testing.T) {
	m := map[string]any{
		"id":     "",      // empty -> skipped
		"agent":  "a-123", // matched
		"status": 42,      // non-string -> skipped
	}
	if got := firstString(m, "missing", "id", "agent"); got != "a-123" {
		t.Errorf("got %q, want a-123", got)
	}
	if got := firstString(m, "status"); got != "" {
		t.Errorf("non-string should yield empty, got %q", got)
	}
	if got := firstString(m, "nope"); got != "" {
		t.Errorf("no match should yield empty, got %q", got)
	}
}

func TestOrUnknown(t *testing.T) {
	if orUnknown("") != "unknown" {
		t.Error("empty -> unknown")
	}
	if orUnknown("connected") != "connected" {
		t.Error("non-empty passthrough")
	}
}

func TestIsConnected(t *testing.T) {
	for _, s := range []string{"connected", "ONLINE", " ready ", "ok"} {
		if !isConnected(s) {
			t.Errorf("isConnected(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "pending", "disconnected", "error"} {
		if isConnected(s) {
			t.Errorf("isConnected(%q) = true, want false", s)
		}
	}
}

func TestRunnerFromState(t *testing.T) {
	t.Run("no state errors", func(t *testing.T) {
		isolate(t)
		if _, err := runnerFromState(); err == nil || !strings.Contains(err.Error(), "no collector installed") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("hydrates runner from saved state", func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{
			ContainerName: "dbg-collector",
			Image:         "img:1",
			ConfigPath:    "/c.toml",
			EnvFilePath:   "/c.env",
			CACertPath:    "/ca.pem",
		}); err != nil {
			t.Fatal(err)
		}
		r, err := runnerFromState()
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if r.Name != "dbg-collector" || r.Image != "img:1" || r.CACertPath != "/ca.pem" {
			t.Errorf("runner = %+v", r)
		}
	})
}

func targetCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("db-host", "localhost", "")
	c.Flags().Int("db-port", 5432, "")
	c.Flags().String("db-user", "", "")
	c.Flags().String("name", "", "")
	c.Flags().String("ssl-mode", "verify-full", "")
	c.Flags().String("db-name", "", "")
	return c
}

func TestResolveTarget(t *testing.T) {
	t.Run("flags fully specified, no prompt", func(t *testing.T) {
		isolate(t)
		c := targetCmd()
		_ = c.Flags().Set("db-user", "ro")
		_ = c.Flags().Set("name", "prod-db")
		_ = c.Flags().Set("db-name", "app, analytics ,")
		tgt, err := resolveTarget(c)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if tgt.User != "ro" || tgt.Name != "prod-db" {
			t.Errorf("target = %+v", tgt)
		}
		if len(tgt.Databases) != 2 || tgt.Databases[0] != "app" || tgt.Databases[1] != "analytics" {
			t.Errorf("databases = %v (empty entries should be trimmed out)", tgt.Databases)
		}
	})

	t.Run("prompts for user + defaults name to user", func(t *testing.T) {
		isolate(t)
		setStdin(t, "promptuser\n\n") // user line, then empty name line -> defaults to user
		c := targetCmd()
		var tgt collector.Target
		var err error
		_ = capture(t, func() { tgt, err = resolveTarget(c) })
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if tgt.User != "promptuser" || tgt.Name != "promptuser" {
			t.Errorf("target = %+v", tgt)
		}
	})

	t.Run("missing user errors", func(t *testing.T) {
		isolate(t)
		setStdin(t, "\n") // empty user prompt
		var err error
		_ = capture(t, func() { _, err = resolveTarget(targetCmd()) })
		if err == nil || !strings.Contains(err.Error(), "database user is required") {
			t.Fatalf("err=%v", err)
		}
	})
}

func passwordCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("db-password", "", "")
	return c
}

func TestResolveDBPassword(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		isolate(t)
		c := passwordCmd()
		_ = c.Flags().Set("db-password", "flagpw")
		pw, err := resolveDBPassword(c)
		if err != nil || pw != "flagpw" {
			t.Fatalf("pw=%q err=%v", pw, err)
		}
	})

	t.Run("env var used when flag empty", func(t *testing.T) {
		isolate(t)
		t.Setenv(collector.DBPasswordEnv, "envpw")
		pw, err := resolveDBPassword(passwordCmd())
		if err != nil || pw != "envpw" {
			t.Fatalf("pw=%q err=%v", pw, err)
		}
	})

	t.Run("no flag/env and non-tty stdin errors", func(t *testing.T) {
		isolate(t)
		setStdin(t, "")
		var err error
		_ = capture(t, func() { _, err = resolveDBPassword(passwordCmd()) })
		if err == nil || !strings.Contains(err.Error(), "cannot read password") {
			t.Fatalf("err=%v, want read failure", err)
		}
	})
}

func TestCheckReachable(t *testing.T) {
	t.Run("open listener is reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		if err := checkReachable(ln.Addr().String()); err != nil {
			t.Errorf("want reachable, got %v", err)
		}
	})

	t.Run("closed port is unreachable", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		_ = ln.Close() // now nothing is listening on addr
		if err := checkReachable(addr); err == nil {
			t.Error("want unreachable error")
		}
	})
}

func TestPrintPreflight(t *testing.T) {
	rep := preflight.Report{Results: []preflight.Result{
		{Name: "connect", Severity: preflight.OK, Detail: "ok"},
		{Name: "grants", Severity: preflight.Warn, Detail: "missing", Fix: []string{"GRANT pg_read_all_stats"}},
		{Name: "version", Severity: preflight.Fail, Detail: "too old"},
	}}
	out := capture(t, func() { printPreflight(rep) })
	if !strings.Contains(out, "✓ connect") || !strings.Contains(out, "⚠ grants") || !strings.Contains(out, "✗ version") {
		t.Errorf("severity markers wrong:\n%s", out)
	}
	if !strings.Contains(out, "GRANT pg_read_all_stats") {
		t.Errorf("fix hint not rendered:\n%s", out)
	}
}

func TestVerifyConnection_ConnectsImmediately(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `{"status":"connected"}`)
	defer srv.Close()
	client := api.NewClient(srv.URL)
	out := capture(t, func() { verifyConnection(client, "agent-1", "") })
	if !strings.Contains(out, "Collector connected (connected)") {
		t.Errorf("output = %q", out)
	}
}
