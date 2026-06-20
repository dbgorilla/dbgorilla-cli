// Package api wraps HTTP calls to the DBGorilla backend.
//
// v0.1.0 surface is minimal -- only the endpoints needed for login, identity
// lookup, and MCP API key management. All requests carry a Bearer token from
// the keychain when one is present; refresh-on-401 happens automatically via
// the refresh token if available.
//
// Security notes:
//   - When the client is not in insecure mode, the redirect policy refuses
//     to follow a redirect that would downgrade to a non-https URL.
//     This prevents a malicious server from steering a Bearer-bearing
//     request to plaintext (which Go's stdlib already strips Authorization
//     on for cross-host redirects, but a same-host http downgrade would
//     still expose other custom headers).
//   - The User-Agent advertises the CLI version so backend abuse-detection
//     and forensic logs can identify the client.
package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
)

// Version is overridden by the cmd package at init via SetUserAgentVersion.
// We can't import cmd from here (cycle), so the cmd package sets this on init.
var userAgentVersion = "dev"

// SetUserAgentVersion lets cmd inject the build-time version string into the
// User-Agent header at startup. Safe to call from any goroutine before the
// first request.
func SetUserAgentVersion(v string) {
	if v != "" {
		userAgentVersion = v
	}
}

// Shared transports for the lifetime of the process. Multiple api.Client
// instances created in one invocation (doctor builds two) share connection
// pools so multi-call commands amortize the TLS handshake. Built lazily and
// only once via sync.Once.
var (
	transportOnce         sync.Once
	transportSecure       *http.Transport
	transportInsecureOnce sync.Once
	transportInsecure     *http.Transport
)

func sharedTransport(insecure bool) *http.Transport {
	if insecure {
		transportInsecureOnce.Do(func() {
			transportInsecure = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			}
		})
		return transportInsecure
	}
	transportOnce.Do(func() {
		transportSecure = &http.Transport{}
	})
	return transportSecure
}

// Client wraps HTTP calls to the DBGorilla backend API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates an API client for the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: buildHTTPClient(false),
	}
}

// NewInsecureClient skips TLS certificate verification. Use only for
// internal/dev environments with self-signed certs.
func NewInsecureClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: buildHTTPClient(true),
	}
}

// buildHTTPClient constructs the underlying http.Client. The Transport is
// shared across all Client instances for the lifetime of the process so a
// command that builds multiple Clients (e.g. doctor) reuses TCP+TLS
// connections instead of re-handshaking. CheckRedirect refuses non-https
// redirects unless insecure.
func buildHTTPClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: sharedTransport(insecure),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !insecure && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-https URL: %s", req.URL.Redacted())
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// Do performs an authenticated HTTP request. Returns the response body
// bytes, status code, and any error.
func (c *Client) Do(method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("cannot serialize request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dbgorilla-cli/"+userAgentVersion)

	tokens, err := auth.LoadTokens()
	if err == nil && tokens != nil {
		if tokens.IsExpired() && tokens.RefreshToken != "" {
			tokens, err = c.refreshTokens(tokens)
			if err != nil {
				// Refresh failed -- caller will get a 401 or need to re-login.
				// Surface this to stderr so a silent keychain-write failure
				// during refresh doesn't lock the user out with no signal.
				fmt.Fprintf(os.Stderr, "warning: token refresh failed: %v\n", err)
				tokens = nil
			}
		}
		if tokens != nil {
			req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		}
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// Get performs an authenticated GET request.
func (c *Client) Get(path string) ([]byte, int, error) {
	return c.Do(http.MethodGet, path, nil)
}

// Post performs an authenticated POST request.
func (c *Client) Post(path string, body any) ([]byte, int, error) {
	return c.Do(http.MethodPost, path, body)
}

// --- Response types -------------------------------------------------------

// UserInfo matches GET /api/v0_1/auth/user on backend release-202603.007.
// `tenant` is the organization display name; `tenant_id` is the UUID.
type UserInfo struct {
	Username       string `json:"username"`
	Email          string `json:"email"`
	Tenant         string `json:"tenant"`
	UserID         string `json:"user_id"`
	TenantID       string `json:"tenant_id"`
	IsAdmin        bool   `json:"is_admin"`
	IsSystemTenant bool   `json:"is_system_tenant"`
}

// ErrorResponse is the standard FastAPI error response.
type ErrorResponse struct {
	Detail string `json:"detail"`
}

// --- Token refresh --------------------------------------------------------

// refreshTokens exchanges a refresh token for a new token pair.
//
// Device/SSO-flow tokens are issued by Keycloak and must be refreshed there
// (a standard OAuth refresh_token grant) — the backend's /token/refresh only
// validates backend-issued, password-flow tokens, so sending it a Keycloak
// refresh token always 401s. We branch on old.TokenEndpoint, which the device
// flow records at login.
//
// Surface storage errors so a silent keychain-write failure doesn't lead
// to repeated refresh attempts on stale tokens.
func (c *Client) refreshTokens(old *auth.Tokens) (*auth.Tokens, error) {
	if old.TokenEndpoint != "" {
		return c.refreshViaKeycloak(old)
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v0_1/auth/token/refresh", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+old.RefreshToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dbgorilla-cli/"+userAgentVersion)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}
	newTokens := &auth.Tokens{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}
	if err := auth.StoreTokens(newTokens); err != nil {
		return nil, fmt.Errorf("token refresh succeeded but storing the new tokens failed: %w", err)
	}
	return newTokens, nil
}

// refreshViaKeycloak renews a device/SSO-flow session using the OAuth
// refresh_token grant at the Keycloak token endpoint recorded at login.
// Keycloak rotates refresh tokens, so the new one is persisted; the token
// endpoint and client id are carried forward for the next refresh.
func (c *Client) refreshViaKeycloak(old *auth.Tokens) (*auth.Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", old.ClientID)
	form.Set("refresh_token", old.RefreshToken)

	req, err := http.NewRequest(http.MethodPost, old.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dbgorilla-cli/"+userAgentVersion)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}
	// Keycloak rotates the refresh token; keep the new one, falling back to the
	// prior token only if the response omits it.
	refresh := tokenResp.RefreshToken
	if refresh == "" {
		refresh = old.RefreshToken
	}
	newTokens := &auth.Tokens{
		AccessToken:   tokenResp.AccessToken,
		RefreshToken:  refresh,
		ExpiresAt:     time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		TokenEndpoint: old.TokenEndpoint,
		ClientID:      old.ClientID,
	}
	if err := auth.StoreTokens(newTokens); err != nil {
		return nil, fmt.Errorf("token refresh succeeded but storing the new tokens failed: %w", err)
	}
	return newTokens, nil
}
