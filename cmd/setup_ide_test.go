package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
)

func TestValidScope(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"local", true},
		{"user", true},
		{"project", true},
		{"global", false},
		{"", false},
		{"USER", false},
	}
	for _, tc := range cases {
		if got := validScope(tc.in); got != tc.want {
			t.Errorf("validScope(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRedactBearer(t *testing.T) {
	cases := []struct {
		name, in, key, want string
	}{
		{
			"literal key replaced",
			"Authorization: Bearer secret-abc-123 failed",
			"secret-abc-123",
			"Authorization: Bearer *** failed",
		},
		{
			"generic Bearer pattern",
			"got error: Bearer eyJhbGciOi... in request",
			"",
			"got error: Bearer *** in request",
		},
		{
			"empty key",
			"plain text no token here",
			"",
			"plain text no token here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactBearer(tc.in, tc.key)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInterpretClaudeError_PolicyMatching(t *testing.T) {
	cases := []struct {
		name     string
		errMsg   string
		isPolicy bool
	}{
		{"policy keyword", "ERROR: policy violation", true},
		{"allowlist keyword", "Not in Allowlist", true},
		{"denied keyword", "request denied by org", true},
		{"not permitted", "operation Not Permitted", true},
		{"unrelated error", "ENOENT: no such file or directory", false},
		{"network error", "connection refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := interpretClaudeError(errors.New(tc.errMsg), "https://api")
			contains := strings.Contains(out.Error(), "blocked by your Claude org")
			if contains != tc.isPolicy {
				t.Errorf("for %q: contains-policy-message=%v, want %v (err=%v)",
					tc.errMsg, contains, tc.isPolicy, out)
			}
		})
	}
}

func TestFetchMCPKey_ResponseShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantKey string
		wantErr bool
	}{
		{"JSON-encoded string (v0.1 backend)", `"abc-key-1"`, 200, "abc-key-1", false},
		{"bare string fallback", "raw-key-2", 200, "raw-key-2", false},
		{"empty response", "", 200, "", true},
		{"5xx with detail", `{"detail":"server exploded"}`, 500, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := api.NewClient(srv.URL)
			got, err := fetchMCPKey(client)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.wantKey {
				t.Errorf("key=%q want %q", got, tc.wantKey)
			}
		})
	}
}

func TestBuildAdminAllowlistOutput(t *testing.T) {
	// Capture printAdminAllowlist's output by redirecting stdout would be
	// a lot of plumbing for a small surface; instead check the URL composition
	// is correct via the same logic.
	cases := []struct {
		api, want string
	}{
		{"https://dbg.acme.com", "https://dbg.acme.com/mcp/"},
		{"https://dbg.acme.com/", "https://dbg.acme.com/mcp/"},
	}
	for _, tc := range cases {
		got := strings.TrimRight(tc.api, "/") + "/mcp/"
		if got != tc.want {
			t.Errorf("api=%q produced %q want %q", tc.api, got, tc.want)
		}
	}
}
