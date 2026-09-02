package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/zalando/go-keyring"
)

// --- shared test helpers --------------------------------------------------

// hermeticAuth isolates the token store so a request built by Do never reads
// the developer's real Keychain or ~/.config credentials file. keyring.MockInit
// swaps in an empty in-memory provider; pointing XDG_CONFIG_HOME at a temp dir
// makes the file-fallback lookup miss cleanly.
func hermeticAuth(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// serverReturning spins an httptest server that answers every request with a
// fixed status and body. Registered for cleanup.
func serverReturning(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadServerURL returns the URL of a server that has already been shut down,
// so any dial gets connection-refused immediately (deterministic, no sleep).
func deadServerURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL
	srv.Close()
	return u
}

// truncatedBodyServer declares a Content-Length far larger than the bytes it
// actually writes, so the client's io.ReadAll aborts with an unexpected-EOF
// mid-stream error. Deterministic (the short write happens synchronously in the
// handler) and requires no sleep.
func truncatedBodyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureStderr redirects os.Stderr for the duration of fn and returns what was
// written. Used to assert the refresh-failure warning without polluting output.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// --- SetUserAgentVersion --------------------------------------------------

func TestSetUserAgentVersion(t *testing.T) {
	orig := userAgentVersion
	t.Cleanup(func() { userAgentVersion = orig })

	SetUserAgentVersion("1.2.3")
	if userAgentVersion != "1.2.3" {
		t.Fatalf("expected version set to 1.2.3, got %q", userAgentVersion)
	}
	// Empty string must be ignored so a blank build var can't erase the default.
	SetUserAgentVersion("")
	if userAgentVersion != "1.2.3" {
		t.Fatalf("empty version should be ignored, got %q", userAgentVersion)
	}
}

// --- constructors & TLS behavior ------------------------------------------

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("https://example.test")
	if c.BaseURL != "https://example.test" {
		t.Fatalf("BaseURL not set: %q", c.BaseURL)
	}
	if c.HTTPClient == nil || c.HTTPClient.Timeout != 30*time.Second {
		t.Fatalf("expected 30s timeout http client, got %+v", c.HTTPClient)
	}
}

// NewInsecureClient must skip TLS verification so a self-signed dev cert works.
func TestNewInsecureClientAcceptsSelfSignedTLS(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	c := NewInsecureClient(srv.URL)
	body, status, err := c.Get("/anything")
	if err != nil {
		t.Fatalf("insecure client against TLS server: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected response: status=%d body=%q", status, body)
	}
}

// The secure client must reject the httptest server's self-signed certificate.
func TestSecureClientRejectsSelfSignedTLS(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, _, err := c.Get("/x"); err == nil {
		t.Fatal("expected TLS verification error against self-signed cert, got nil")
	}
}

// --- Do: request building & error mapping ---------------------------------

func TestDoSerializeBodyError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient("https://example.test")
	// A channel can't be JSON-encoded, so marshaling fails before any request.
	_, _, err := c.Post("/x", make(chan int))
	if err == nil || !strings.Contains(err.Error(), "cannot serialize request body") {
		t.Fatalf("expected serialize error, got %v", err)
	}
}

func TestDoNewRequestError(t *testing.T) {
	hermeticAuth(t)
	// A control character in the URL makes http.NewRequest fail at url.Parse.
	c := NewClient("http://bad\x7fhost")
	_, _, err := c.Get("/x")
	if err == nil || !strings.Contains(err.Error(), "cannot create request") {
		t.Fatalf("expected request-creation error, got %v", err)
	}
}

func TestDoTransportError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient(deadServerURL(t))
	_, _, err := c.Get("/x")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("expected transport error, got %v", err)
	}
}

// Client-level timeout maps to the "request failed" error. The handler blocks
// on the request context so the only possible outcome is a timeout (no sleep,
// no flakiness — the request can never succeed).
func TestDoClientTimeout(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, _, err := c.Get("/slow")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("expected timeout mapped to request failed, got %v", err)
	}
}

// A body that ends before its declared Content-Length surfaces as a read error
// while still reporting the HTTP status that was received.
func TestDoReadBodyError(t *testing.T) {
	hermeticAuth(t)
	srv := truncatedBodyServer(t)
	_, status, err := NewClient(srv.URL).Get("/x")
	if err == nil || !strings.Contains(err.Error(), "cannot read response") {
		t.Fatalf("expected body-read error, got %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected status to still be reported, got %d", status)
	}
}

func TestDoSetsStandardHeaders(t *testing.T) {
	hermeticAuth(t)
	var gotCT, gotAccept, gotUA, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	body, status, err := c.Post("/echo", map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("unexpected response: %d %q", status, body)
	}
	if gotCT != "application/json" || gotAccept != "application/json" {
		t.Fatalf("content headers wrong: ct=%q accept=%q", gotCT, gotAccept)
	}
	if !strings.HasPrefix(gotUA, "dbgorilla-cli/") {
		t.Fatalf("user-agent not set: %q", gotUA)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header when unauthenticated, got %q", gotAuth)
	}
	if gotBody != `{"hello":"world"}` {
		t.Fatalf("request body not serialized as expected: %q", gotBody)
	}
}

// A stored, non-expired token is attached as a Bearer credential and refresh
// is not triggered.
func TestDoAttachesValidToken(t *testing.T) {
	hermeticAuth(t)
	if err := auth.StoreTokens(&auth.Tokens{
		AccessToken: "live-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("store: %v", err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL).Get("/me"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotAuth != "Bearer live-token" {
		t.Fatalf("expected Bearer live-token, got %q", gotAuth)
	}
}

// An expired token with no refresh token can't be refreshed; the (stale) access
// token is still attached — the backend decides whether to 401.
func TestDoExpiredTokenNoRefreshAttachesStale(t *testing.T) {
	hermeticAuth(t)
	if err := auth.StoreTokens(&auth.Tokens{
		AccessToken: "stale",
		ExpiresAt:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("store: %v", err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL).Get("/me"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotAuth != "Bearer stale" {
		t.Fatalf("expected stale token attached, got %q", gotAuth)
	}
}

// An expired token WITH a refresh token triggers a refresh; the freshly minted
// access token is what gets attached to the outgoing request.
func TestDoRefreshesExpiredTokenBeforeRequest(t *testing.T) {
	hermeticAuth(t)
	if err := auth.StoreTokens(&auth.Tokens{
		AccessToken:  "old",
		RefreshToken: "refresh-me",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("store: %v", err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0_1/auth/token/refresh" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"r2","expires_in":300}`))
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL).Get("/me"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotAuth != "Bearer fresh" {
		t.Fatalf("expected refreshed token attached, got %q", gotAuth)
	}
}

// If refresh fails, Do drops the token (no Authorization header) and warns on
// stderr rather than locking the user out silently.
func TestDoRefreshFailureDropsTokenAndWarns(t *testing.T) {
	hermeticAuth(t)
	if err := auth.StoreTokens(&auth.Tokens{
		AccessToken:  "old",
		RefreshToken: "refresh-me",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("store: %v", err)
	}
	var gotAuth string
	sawAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0_1/auth/token/refresh" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		sawAuth = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	stderr := captureStderr(t, func() {
		if _, status, err := NewClient(srv.URL).Get("/me"); err != nil || status != http.StatusUnauthorized {
			t.Errorf("get: status=%d err=%v", status, err)
		}
	})
	if !sawAuth {
		t.Fatal("target endpoint was never reached")
	}
	if gotAuth != "" {
		t.Fatalf("expected token dropped after failed refresh, got %q", gotAuth)
	}
	if !strings.Contains(stderr, "token refresh failed") {
		t.Fatalf("expected refresh-failure warning on stderr, got %q", stderr)
	}
}

// --- CheckRedirect policy -------------------------------------------------

// A secure client must refuse to follow a redirect that downgrades to http.
func TestCheckRedirectRefusesNonHTTPS(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://downgrade.invalid/steal")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	_, _, err := NewClient(srv.URL).Get("/redirect")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect to non-https") {
		t.Fatalf("expected non-https redirect refusal, got %v", err)
	}
}

// The redirect loop guard trips after 10 hops. Using the insecure client keeps
// the http->http self-redirect from being rejected by the non-https guard first.
func TestCheckRedirectStopsAfterTenRedirects(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	defer srv.Close()

	_, _, err := NewInsecureClient(srv.URL).Get("/")
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("expected redirect-limit error, got %v", err)
	}
}

// --- refreshTokens (backend password-flow path) error branches ------------

func TestRefreshTokensBackendNewRequestError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient("http://bad\x7fhost")
	if _, err := c.refreshTokens(&auth.Tokens{RefreshToken: "r"}); err == nil {
		t.Fatal("expected NewRequest error from malformed URL")
	}
}

func TestRefreshTokensBackendTransportError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient(deadServerURL(t))
	if _, err := c.refreshTokens(&auth.Tokens{RefreshToken: "r"}); err == nil {
		t.Fatal("expected transport error from dead server")
	}
}

func TestRefreshTokensBackendNon200(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusInternalServerError, "boom")
	c := NewClient(srv.URL)
	_, err := c.refreshTokens(&auth.Tokens{RefreshToken: "r"})
	if err == nil || !strings.Contains(err.Error(), "token refresh failed (HTTP 500)") {
		t.Fatalf("expected HTTP 500 refresh error, got %v", err)
	}
}

func TestRefreshTokensBackendBadJSON(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, "not json")
	c := NewClient(srv.URL)
	if _, err := c.refreshTokens(&auth.Tokens{RefreshToken: "r"}); err == nil {
		t.Fatal("expected JSON decode error")
	}
}

func TestRefreshTokensBackendReadError(t *testing.T) {
	hermeticAuth(t)
	srv := truncatedBodyServer(t)
	if _, err := NewClient(srv.URL).refreshTokens(&auth.Tokens{RefreshToken: "r"}); err == nil {
		t.Fatal("expected body-read error during refresh")
	}
}

// --- refreshViaKeycloak (SSO device-flow path) branches -------------------

func TestRefreshViaKeycloakNewRequestError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "r", TokenEndpoint: "http://bad\x7fhost", ClientID: "cli"}
	if _, err := c.refreshTokens(old); err == nil {
		t.Fatal("expected NewRequest error from malformed token endpoint")
	}
}

func TestRefreshViaKeycloakTransportError(t *testing.T) {
	hermeticAuth(t)
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "r", TokenEndpoint: deadServerURL(t), ClientID: "cli"}
	if _, err := c.refreshTokens(old); err == nil {
		t.Fatal("expected transport error from dead token endpoint")
	}
}

func TestRefreshViaKeycloakNon200(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusBadRequest, "invalid_grant")
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "r", TokenEndpoint: srv.URL, ClientID: "cli"}
	_, err := c.refreshTokens(old)
	if err == nil || !strings.Contains(err.Error(), "token refresh failed (HTTP 400)") {
		t.Fatalf("expected HTTP 400 refresh error, got %v", err)
	}
}

func TestRefreshViaKeycloakBadJSON(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, "{not-json")
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "r", TokenEndpoint: srv.URL, ClientID: "cli"}
	if _, err := c.refreshTokens(old); err == nil {
		t.Fatal("expected JSON decode error")
	}
}

func TestRefreshViaKeycloakReadError(t *testing.T) {
	hermeticAuth(t)
	srv := truncatedBodyServer(t)
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "r", TokenEndpoint: srv.URL, ClientID: "cli"}
	if _, err := c.refreshTokens(old); err == nil {
		t.Fatal("expected body-read error during keycloak refresh")
	}
}

// Keycloak may omit refresh_token on rotation-disabled realms; the prior refresh
// token must be carried forward rather than blanked.
func TestRefreshViaKeycloakKeepsOldRefreshWhenOmitted(t *testing.T) {
	hermeticAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a2","expires_in":300}`))
	}))
	defer srv.Close()

	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{RefreshToken: "keep-me", TokenEndpoint: srv.URL, ClientID: "cli"}
	nt, err := c.refreshTokens(old)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if nt.AccessToken != "a2" {
		t.Fatalf("expected new access token a2, got %q", nt.AccessToken)
	}
	if nt.RefreshToken != "keep-me" {
		t.Fatalf("expected prior refresh token retained, got %q", nt.RefreshToken)
	}
}

func TestDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	body, status, err := NewClient(srv.URL).Delete("/api/v0_1/thing")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v0_1/thing" {
		t.Errorf("sent %s %s, want DELETE /api/v0_1/thing", gotMethod, gotPath)
	}
	if status != http.StatusNoContent {
		t.Errorf("status=%d, want 204", status)
	}
	// A 204 has no body; the caller must get an empty slice, not a nil-deref.
	if len(body) != 0 {
		t.Errorf("body=%q, want empty", body)
	}
}
