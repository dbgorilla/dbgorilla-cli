package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/zalando/go-keyring"
)

// SSO/device-flow tokens must refresh at the Keycloak token endpoint via an
// OAuth refresh_token grant — not the backend /token/refresh endpoint (which
// only validates backend-issued tokens and would 401).
func TestRefreshRoutesSSOSessionToKeycloak(t *testing.T) {
	keyring.MockInit()

	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"newA","refresh_token":"newR","expires_in":300}`))
	}))
	defer srv.Close()

	// Backend URL deliberately points nowhere usable: the SSO path must NOT
	// touch it.
	c := NewClient("https://backend.invalid")
	old := &auth.Tokens{
		RefreshToken:  "oldR",
		TokenEndpoint: srv.URL,
		ClientID:      "dbgorilla-cli",
		ExpiresAt:     time.Now().Add(-time.Hour),
	}

	nt, err := c.refreshTokens(old)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if nt.AccessToken != "newA" || nt.RefreshToken != "newR" {
		t.Fatalf("unexpected tokens: %+v", nt)
	}
	if nt.TokenEndpoint != srv.URL || nt.ClientID != "dbgorilla-cli" {
		t.Fatalf("endpoint/client_id not carried forward: %+v", nt)
	}
	if gotForm.Get("grant_type") != "refresh_token" ||
		gotForm.Get("refresh_token") != "oldR" ||
		gotForm.Get("client_id") != "dbgorilla-cli" {
		t.Fatalf("unexpected refresh form: %v", gotForm)
	}
}

// Password-flow tokens (no TokenEndpoint) still refresh against the backend's
// /token/refresh endpoint with the refresh token in the Authorization header.
func TestRefreshPasswordSessionUsesBackend(t *testing.T) {
	keyring.MockInit()

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":300}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	old := &auth.Tokens{RefreshToken: "bk", ExpiresAt: time.Now().Add(-time.Hour)}

	if _, err := c.refreshTokens(old); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotPath != "/api/v0_1/auth/token/refresh" {
		t.Fatalf("expected backend refresh path, got %q", gotPath)
	}
	if gotAuth != "Bearer bk" {
		t.Fatalf("expected refresh token in Authorization header, got %q", gotAuth)
	}
}
