package collector

import (
	"strings"
	"testing"
)

func TestDialHost(t *testing.T) {
	cases := map[string]string{
		"host.docker.internal":   "localhost", // container rewrite reversed
		"[host.docker.internal]": "localhost", // trimmed brackets
		"HOST.DOCKER.INTERNAL":   "localhost", // case-insensitive
		"localhost":              "localhost", // already host-side
		"10.0.0.5":               "10.0.0.5",  // real host unchanged
		"db.example.com":         "db.example.com",
	}
	for in, want := range cases {
		if got := DialHost(in); got != want {
			t.Errorf("DialHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfig_roundTrip(t *testing.T) {
	cfg := Build("agent-1", "tenant-1",
		Target{Name: "Local", Host: "localhost", Port: 5433, Databases: []string{"appdb"}, User: "dbg_collector", SSLMode: "disable"},
		Endpoints{})
	rendered, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := t.TempDir() + "/collector.toml"
	if err := WriteConfig(path, rendered); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Component) != 1 {
		t.Fatalf("want 1 component, got %d", len(got.Component))
	}
	c := got.Component[0].Connect
	// Build rewrites loopback host -> host.docker.internal in the file.
	if c.Host != DockerHostInternal || c.Port != 5433 || len(c.Databases) != 1 || c.Databases[0] != "appdb" {
		t.Errorf("connect round-trip wrong: %+v", c)
	}
	if got.Component[0].Auth.User != "dbg_collector" {
		t.Errorf("user round-trip wrong: %q", got.Component[0].Auth.User)
	}
}

func TestLoadConfig_error(t *testing.T) {
	if _, err := LoadConfig(t.TempDir() + "/does-not-exist.toml"); err == nil {
		t.Error("expected an error loading a missing config")
	}
}

func TestHostDial(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"localhost", 5432, "localhost:5432"},
		{"db.example.com", 6432, "db.example.com:6432"},
		{"::1", 5432, "[::1]:5432"}, // IPv6 gets bracketed by net.JoinHostPort
	}
	for _, tc := range cases {
		got := Target{Host: tc.host, Port: tc.port}.HostDial()
		if got != tc.want {
			t.Errorf("HostDial(%q,%d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestBuild_endpointsPassThroughAndRender(t *testing.T) {
	eps := Endpoints{
		OpampBaseURL:    "https://opamp.internal.example.com",
		OtlpBaseURL:     "https://otlp.internal.example.com:4317",
		KeycloakBaseURL: "https://auth.internal.example.com",
	}
	cfg := Build("agent-2", "tenant-2", Target{
		Name: "billing", Host: "db.example.com", Port: 5432, User: "dbg_ro",
	}, eps)

	if cfg.Dbgorilla.OpampBaseURL != eps.OpampBaseURL ||
		cfg.Dbgorilla.OtlpBaseURL != eps.OtlpBaseURL ||
		cfg.Dbgorilla.KeycloakBaseURL != eps.KeycloakBaseURL {
		t.Errorf("endpoint overrides not carried onto Config: %+v", cfg.Dbgorilla)
	}

	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Explicit overrides must be emitted (unlike the empty-omitempty case).
	for _, want := range []string{
		"opamp_base_url = \"https://opamp.internal.example.com\"",
		"otlp_base_url = \"https://otlp.internal.example.com:4317\"",
		"keycloak_base_url = \"https://auth.internal.example.com\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered TOML missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuild_emptyDatabasesOmitted(t *testing.T) {
	// No databases -> the databases key is omitted (omitempty) from the render.
	cfg := Build("a", "t", Target{Name: "n", Host: "db.example.com", Port: 5432, User: "u"}, Endpoints{})
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "databases") {
		t.Errorf("empty databases slice should be omitted:\n%s", out)
	}
}
