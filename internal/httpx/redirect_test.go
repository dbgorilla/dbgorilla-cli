package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func req(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{URL: u, Method: http.MethodGet}
}

// A deployment that has moved answers every path with a redirect to its new
// home. Following it lands on a web page that returns 200, so a status check
// passes and the failure surfaces much later as a JSON parse error about "<".
func TestRedirectPolicy_RefusesADifferentHost(t *testing.T) {
	policy := RedirectPolicy(false)
	origin := req(t, "https://demo.example.com/api/v0_1/auth/keycloak/device-config")
	err := policy(req(t, "https://app.example.com/"), []*http.Request{origin})

	var crossHost *CrossHostRedirectError
	if !errors.As(err, &crossHost) {
		t.Fatalf("err = %v, want *CrossHostRedirectError", err)
	}
	msg := crossHost.Error()
	// The user has to learn where they were sent and how to follow it on purpose.
	for _, want := range []string{
		"demo.example.com",
		"app.example.com",
		"dbg config set api-url https://app.example.com",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should contain %q, got:\n%s", want, msg)
		}
	}
}

// A redirect within the same host is ordinary routing and must still work.
func TestRedirectPolicy_AllowsSameHost(t *testing.T) {
	policy := RedirectPolicy(false)
	origin := req(t, "https://api.example.com/api/thing")
	if err := policy(req(t, "https://api.example.com/api/thing/"), []*http.Request{origin}); err != nil {
		t.Fatalf("a same-host redirect must be followed, got %v", err)
	}
}

// Host comparison is case-insensitive; DNS is.
func TestRedirectPolicy_HostCompareIgnoresCase(t *testing.T) {
	policy := RedirectPolicy(false)
	origin := req(t, "https://API.example.com/x")
	if err := policy(req(t, "https://api.example.com/y"), []*http.Request{origin}); err != nil {
		t.Fatalf("same host in different case must be allowed, got %v", err)
	}
}

// A different port is a different endpoint, and tokens ride on these requests.
func TestRedirectPolicy_RefusesADifferentPort(t *testing.T) {
	policy := RedirectPolicy(false)
	origin := req(t, "https://api.example.com/x")
	if err := policy(req(t, "https://api.example.com:8443/x"), []*http.Request{origin}); err == nil {
		t.Fatal("a port change must not be followed silently")
	}
}

func TestRedirectPolicy_RefusesTLSDowngrade(t *testing.T) {
	policy := RedirectPolicy(false)
	origin := req(t, "https://api.example.com/x")
	err := policy(req(t, "http://api.example.com/x"), []*http.Request{origin})
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("err = %v, want a refusal to downgrade", err)
	}

	// --insecure exists for self-signed dev backends; it opts out of this.
	if err := RedirectPolicy(true)(req(t, "http://api.example.com/x"), []*http.Request{origin}); err != nil {
		t.Errorf("insecure should permit the downgrade, got %v", err)
	}
}

func TestRedirectPolicy_StopsAnEndlessChain(t *testing.T) {
	policy := RedirectPolicy(false)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = req(t, "https://api.example.com/x")
	}
	if err := policy(req(t, "https://api.example.com/x"), via); err == nil {
		t.Fatal("a 10-deep chain must be stopped")
	}
}

// The very first request has no history to compare against.
func TestRedirectPolicy_EmptyChainIsAllowed(t *testing.T) {
	if err := RedirectPolicy(false)(req(t, "https://api.example.com/x"), nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// End to end through a real client: the redirect is refused, and the reason
// survives Go's wrapping so a caller can errors.As it.
func TestRedirectPolicy_ThroughARealClient(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body>login</body></html>"))
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	// Test servers are http, so allow the scheme and let the host rule bite.
	client := &http.Client{CheckRedirect: RedirectPolicy(true)}
	_, err := client.Get(origin.URL + "/api/v0_1/auth/keycloak/device-config")
	if err == nil {
		t.Fatal("expected the redirect to be refused")
	}
	var crossHost *CrossHostRedirectError
	if !errors.As(err, &crossHost) {
		t.Fatalf("err = %v, want the cause to survive as *CrossHostRedirectError", err)
	}
}

func TestIsHTML(t *testing.T) {
	for _, body := range []string{
		"<!doctype html>\n<html lang=\"en\">",
		"<!DOCTYPE HTML>",
		"<html><body>hi</body></html>",
		"\n\n  <html>",
		"<?xml version=\"1.0\"?><head></head>",
	} {
		if !IsHTML([]byte(body)) {
			t.Errorf("should be recognised as HTML: %q", body)
		}
	}

	// JSON must never be mistaken for a web page, including JSON that merely
	// mentions a tag inside a string value.
	for _, body := range []string{
		`{"client_id":"dbg"}`,
		`{"detail":"<head> is not allowed here"}`,
		"",
		"plain text",
	} {
		if IsHTML([]byte(body)) {
			t.Errorf("must NOT be treated as HTML: %q", body)
		}
	}
}
