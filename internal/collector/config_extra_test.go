package collector

import (
	"strings"
	"testing"
)

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
