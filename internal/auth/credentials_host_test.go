package auth

import "testing"

// A token is for one deployment. --api-url can name any host, and a token
// sent to the wrong one is handed to whoever runs it.
func TestTokensIssuedFor(t *testing.T) {
	cases := []struct {
		name, issued, target string
		want                 bool
	}{
		{"same host", "https://x.example", "https://x.example", true},
		{"same host, path and case differ", "https://X.example/", "https://x.example/api", true},
		{"different host", "https://x.example", "https://y.example", false},
		{"different port", "https://x.example:8443", "https://x.example", false},
		{"legacy session with no host is accepted", "", "https://anything.example", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok := &Tokens{AccessToken: "a", APIURL: c.issued}
			if got := tok.IssuedFor(c.target); got != c.want {
				t.Errorf("IssuedFor(%q) with APIURL %q = %v, want %v", c.target, c.issued, got, c.want)
			}
		})
	}
}
