package collector

import (
	"strings"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true,
		"LOCALHOST": true, " localhost ": true,
		"db.internal.example.com": false, "10.0.0.5": false, "host.docker.internal": false,
	}
	for host, want := range cases {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestBuildRewritesLoopbackAndRefsSecrets(t *testing.T) {
	cfg := Build("agent-1", "tenant-1", Target{
		Name: "orders", Host: "localhost", Port: 5432,
		Databases: []string{"orders"}, User: "dbg_ro", SSLMode: "require",
	}, Endpoints{})

	c := cfg.Component[0].Connect
	if c.Host != DockerHostInternal {
		t.Errorf("loopback host not rewritten: got %q", c.Host)
	}
	if cfg.Dbgorilla.Secret != "${"+SecretEnv+"}" {
		t.Errorf("secret should be an env ref, got %q", cfg.Dbgorilla.Secret)
	}
	if cfg.Component[0].Auth.Password != "${"+DBPasswordEnv+"}" {
		t.Errorf("password should be an env ref, got %q", cfg.Component[0].Auth.Password)
	}
}

func TestBuildKeepsNonLoopbackHostAndDefaultsSSL(t *testing.T) {
	cfg := Build("a", "t", Target{
		Name: "n", Host: "db.example.com", Port: 5432, User: "u",
	}, Endpoints{})
	if cfg.Component[0].Connect.Host != "db.example.com" {
		t.Errorf("non-loopback host changed: %q", cfg.Component[0].Connect.Host)
	}
	if cfg.Component[0].Connect.SSLMode != "verify-full" {
		t.Errorf("ssl_mode should default to verify-full, got %q", cfg.Component[0].Connect.SSLMode)
	}
}

func TestRenderProducesExpectedTOML(t *testing.T) {
	cfg := Build("agent-1", "tenant-1", Target{
		Name: "orders", Host: "localhost", Port: 5432,
		Databases: []string{"orders"}, User: "dbg_ro", SSLMode: "verify-full",
	}, Endpoints{})
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"[dbgorilla]", `agent_id = "agent-1"`, `secret = "${DBG_SERVER_SECRET}"`,
		"[[component]]", `engine = "postgres"`, "[component.provider]",
		`host = "host.docker.internal"`, `password = "${COLLECTOR_DB_PASSWORD}"`,
		"[topology]", "[commands]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered TOML missing %q\n---\n%s", want, out)
		}
	}
	// Empty endpoint overrides must be omitted, not emitted blank.
	if strings.Contains(out, "opamp_base_url") {
		t.Errorf("empty endpoint override should be omitted:\n%s", out)
	}
}
