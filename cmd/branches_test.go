package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// --- collector lifecycle commands (logs / start / stop / restart) ----------

func TestCollectorLifecycle_NoState(t *testing.T) {
	commands := map[string]func() error{
		"start":   func() error { return startCmd.RunE(startCmd, nil) },
		"stop":    func() error { return stopCmd.RunE(stopCmd, nil) },
		"restart": func() error { return restartCmd.RunE(restartCmd, nil) },
		"logs":    func() error { return logsCmd.RunE(logsCmd, nil) },
	}
	for name, run := range commands {
		t.Run(name+" without an install errors", func(t *testing.T) {
			isolate(t)
			if err := run(); err == nil || !strings.Contains(err.Error(), "no collector installed") {
				t.Fatalf("%s: err=%v", name, err)
			}
		})
	}
}

func TestCollectorLifecycle_DockerFailurePropagates(t *testing.T) {
	// With an install recorded but Docker unavailable (empty PATH), each
	// lifecycle command should surface the docker failure rather than succeed.
	commands := map[string]func() error{
		"start":   func() error { return startCmd.RunE(startCmd, nil) },
		"stop":    func() error { return stopCmd.RunE(stopCmd, nil) },
		"restart": func() error { return restartCmd.RunE(restartCmd, nil) },
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if err := collector.SaveState(&collector.State{ContainerName: "dbg-collector"}); err != nil {
				t.Fatal(err)
			}
			if err := run(); err == nil {
				t.Fatalf("%s should fail when docker is unavailable", name)
			}
		})
	}
}

// --- runStatus connection sub-states --------------------------------------

func TestRunStatus_ConnectionStates(t *testing.T) {
	t.Run("not yet seen by control plane", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := statusServer(t, 404, "") // FetchCollectorStatus 404 -> (nil, nil)
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
		if !strings.Contains(out, "not yet seen by control plane") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("connection unknown when control plane unreachable", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		if err := collector.SaveState(&collector.State{AgentID: "a1", ContainerName: "dbg-collector"}); err != nil {
			t.Fatal(err)
		}
		c := baseCmd()
		mustSet(t, c, "api-url", "http://127.0.0.1:1")
		out := capture(t, func() {
			if err := runStatus(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Connection: unknown") {
			t.Errorf("out=%q", out)
		}
	})
}

// --- runUninstall: backend deprovision failure keeps local state -----------

func TestRunUninstall_DeprovisionFailureKeepsState(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 500, `{"detail":"still in use"}`)
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
	if !strings.Contains(out, "could not deprovision identity") {
		t.Errorf("out=%q", out)
	}
	if st, _ := collector.LoadState(); st == nil {
		t.Error("state must be kept when deprovision fails, so the user can retry")
	}
}

// --- endpointsFor: keycloak + opamp overrides ------------------------------

func TestEndpointsFor_AllOverrides(t *testing.T) {
	c := endpointFlagCmd()
	_ = c.Flags().Set("keycloak-url", "https://kc.override")
	_ = c.Flags().Set("otlp-url", "https://otlp.override")
	_ = c.Flags().Set("opamp-url", "https://opamp.override")
	e := endpointsFor(&api.CollectorCredentials{
		KeycloakBaseURL: "https://mint-kc",
		OtlpBaseURL:     "https://mint-otlp",
		OpampBaseURL:    "https://mint-opamp",
	}, c)
	if e.KeycloakBaseURL != "https://kc.override" || e.OpampBaseURL != "https://opamp.override" {
		t.Errorf("flag overrides not applied: %+v", e)
	}
	if e.OtlpBaseURL != "https://otlp.override:443" {
		t.Errorf("otlp override + default port wrong: %q", e.OtlpBaseURL)
	}
}

// --- setup-ide writer error / update branches ------------------------------

func TestRunSetupIDE_ConfigPathError(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"k"`)
	defer srv.Close()
	stubDetect(t, fakeWriter{name: "Broken", slug: "broken", pathErr: os.ErrPermission})
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	var err error
	out := capture(t, func() { err = runSetupIDE(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "failed to configure") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(out, "error resolving config path") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_JSONCRefused(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"k"`)
	defer srv.Close()
	jsoncPath := filepath.Join(t.TempDir(), "config.jsonc")
	stubDetect(t, fakeWriter{name: "Commented", slug: "commented", path: jsoncPath})
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	out := capture(t, func() { _ = runSetupIDE(c, nil) })
	if !strings.Contains(out, "Refused to overwrite JSONC config") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_BareAdapterSkipped(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"k"`)
	defer srv.Close()
	stubDetect(t, fakeBare{name: "Bare", slug: "bare"})
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "No setup path implemented") {
		t.Errorf("out=%q", out)
	}
}

func TestRunSetupIDE_UpdatesExistingEntry(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 200, `"new-key"`)
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "mcp.json")
	// Pre-existing dbgorilla entry with a stale key -> triggers an in-place update.
	if err := os.WriteFile(cfgPath,
		[]byte(`{"mcpServers":{"dbgorilla":{"url":"https://old/mcp/","headers":{"Authorization":"Bearer old"}}}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	stubDetect(t, fakeWriter{name: "Cfg", slug: "cfg", path: cfgPath})
	c := setupIDETestCmd()
	mustSet(t, c, "api-url", srv.URL)
	out := capture(t, func() {
		if err := runSetupIDE(c, nil); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	if !strings.Contains(out, "Updated existing dbgorilla entry") {
		t.Errorf("out=%q", out)
	}
}
