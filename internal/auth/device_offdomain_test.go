package auth

import (
	"bytes"
	"strings"
	"testing"
)

// The warning is for one thing: sign-in being handed to a party other than the
// one running the API. An identity provider on a sibling subdomain is the
// ordinary deployment, and printing an alarming block for it on the first
// command a new user runs teaches people not to read the alarming blocks.
func TestWarnOffDomainEndpoints_Silent(t *testing.T) {
	cases := []struct {
		name, apiURL string
		hosts        []string
	}{
		{
			"identity provider on a sibling subdomain",
			"https://api.example.com",
			[]string{"auth.example.com", "auth.example.com"},
		},
		{
			"deeper subdomain of the same registrable domain",
			"https://api.eu.example.com",
			[]string{"login.idp.example.com"},
		},
		{
			"exactly the API host",
			"https://example.com",
			[]string{"example.com"},
		},
		{
			"same host on another port",
			"https://api.example.com",
			[]string{"api.example.com"},
		},
		{
			"a development box, where everything is localhost",
			"http://localhost:8080",
			[]string{"localhost"},
		},
		{
			"no endpoints to judge",
			"https://api.example.com",
			nil,
		},
		{
			"an API URL that does not parse as one",
			"://nonsense",
			[]string{"idp.attacker.net"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnOffDomainEndpoints(&buf, tc.apiURL, tc.hosts...)
			if buf.Len() != 0 {
				t.Errorf("want silence, got %q", buf.String())
			}
		})
	}
}

func TestWarnOffDomainEndpoints_Warns(t *testing.T) {
	t.Run("a different registrable domain", func(t *testing.T) {
		var buf bytes.Buffer
		warnOffDomainEndpoints(&buf, "https://api.example.com", "idp.attacker.net")
		out := buf.String()
		if !strings.Contains(out, "idp.attacker.net") {
			t.Errorf("must name the host it would talk to: %q", out)
		}
		if !strings.Contains(out, "example.com") {
			t.Errorf("must name the domain being left: %q", out)
		}
		if got := strings.Count(strings.TrimRight(out, "\n"), "\n"); got != 0 {
			t.Errorf("want one line, got %d extra: %q", got, out)
		}
	})

	// Both endpoints normally point at the same identity provider. Saying it
	// twice is the noise, not the signal.
	t.Run("one line for two endpoints on the same host", func(t *testing.T) {
		var buf bytes.Buffer
		warnOffDomainEndpoints(&buf, "https://api.example.com",
			"idp.attacker.net", "idp.attacker.net")
		if got := strings.Count(buf.String(), "\n"); got != 1 {
			t.Errorf("want exactly one line, got %d: %q", got, buf.String())
		}
	})

	t.Run("two genuinely different hosts each get a line", func(t *testing.T) {
		var buf bytes.Buffer
		warnOffDomainEndpoints(&buf, "https://api.example.com",
			"idp.attacker.net", "tokens.elsewhere.org")
		out := buf.String()
		if !strings.Contains(out, "idp.attacker.net") || !strings.Contains(out, "tokens.elsewhere.org") {
			t.Errorf("both hosts must be named: %q", out)
		}
		if got := strings.Count(out, "\n"); got != 2 {
			t.Errorf("want two lines, got %d: %q", got, out)
		}
	})

	// The reason this uses a public-suffix list rather than "compare the last
	// two labels". Under a multi-label public suffix the shortcut compares
	// "co.uk" to "co.uk", calls two unrelated companies the same party, and
	// goes silent for exactly the handover the warning exists to catch.
	t.Run("unrelated companies under a multi-label public suffix", func(t *testing.T) {
		var buf bytes.Buffer
		warnOffDomainEndpoints(&buf, "https://api.yourcompany.co.uk", "idp.attacker.co.uk")
		if buf.Len() == 0 {
			t.Error("attacker.co.uk is not yourcompany.co.uk; this must warn")
		}
	})

	// Nothing establishes that two different IP literals are the same party.
	t.Run("different bare hosts", func(t *testing.T) {
		var buf bytes.Buffer
		warnOffDomainEndpoints(&buf, "https://10.0.0.1", "10.0.0.2")
		if buf.Len() == 0 {
			t.Error("a different address is a different host; this must warn")
		}
	})
}

func TestSameRegistrableDomain(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"auth.example.com", "api.example.com", true},
		{"example.com", "api.example.com", true},
		{"API.Example.COM", "auth.example.com", true}, // hostnames are case-insensitive
		{"idp.attacker.net", "api.example.com", false},
		{"attacker.co.uk", "yourcompany.co.uk", false},
		{"localhost", "localhost", true},
		{"localhost", "otherbox", false},
	}
	for _, tc := range cases {
		if got := sameRegistrableDomain(tc.a, tc.b); got != tc.want {
			t.Errorf("sameRegistrableDomain(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDescribeDomain(t *testing.T) {
	if got := describeDomain("api.eu.example.com"); got != "example.com" {
		t.Errorf("got %q, want the registrable domain", got)
	}
	// Nothing shorter to say about a bare name, so say the name.
	if got := describeDomain("localhost"); got != "localhost" {
		t.Errorf("got %q, want the host itself", got)
	}
}
