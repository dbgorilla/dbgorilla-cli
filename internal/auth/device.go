// Device flow (RFC 8628) login against Keycloak via the DBGorilla backend.
//
// Flow:
//
//  1. Discover -- GET {api}/api/v0_1/auth/keycloak/device-config (public).
//     Returns the Keycloak device authorization endpoint, token endpoint,
//     client_id ("dbgorilla-cli"), and verification_uri.
//
//  2. Request device code -- POST to device_authorization_endpoint with
//     client_id. Returns {device_code, user_code, verification_uri,
//     verification_uri_complete, expires_in, interval}.
//
//  3. Display user_code + verification_uri to the user; try to open the
//     browser at verification_uri_complete (which has the code already
//     filled in) so the user just clicks "approve".
//
//  4. Poll token_endpoint until 200 (success), or an error other than
//     authorization_pending / slow_down terminates the flow.
//
// On a headless machine the browser-open is a no-op; the user copies the
// printed code+URL elsewhere. Same code path either way.
//
// Security notes:
//   - The discovered endpoints (device_authorization, token) are validated
//     to use https unless --insecure is set. This prevents a malicious
//     backend (or one with a typoed config) from silently downgrading the
//     polling step. We do NOT enforce host-equality with apiURL because
//     Keycloak is often deployed on a separate subdomain -- but we warn
//     loudly when the host differs, so an attacker can't quietly redirect
//     polling to a domain they control.
//   - When !insecure, the HTTP client refuses to follow redirects to a
//     non-https URL, preventing TLS downgrade via redirect.
package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"
)

// DeviceConfig matches GET /api/v0_1/auth/keycloak/device-config.
type DeviceConfig struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	ClientID                    string `json:"client_id"`
	VerificationURI             string `json:"verification_uri"`
}

// deviceCodeResponse is the per-RFC-8628 device authorization response.
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// IsDeviceFlowAvailable returns true if the backend at apiURL exposes the
// Keycloak device-config endpoint. Used by `dbg login` to auto-pick mode.
// Network errors return false (fall back to password mode).
func IsDeviceFlowAvailable(ctx context.Context, apiURL string, insecure bool) bool {
	_, err := DiscoverDeviceConfig(ctx, apiURL, insecure)
	return err == nil
}

// DiscoverDeviceConfig fetches the device-config from the backend and
// validates the returned endpoints. Returns an error if any required field
// is missing or (when !insecure) any endpoint URL uses a non-https scheme.
func DiscoverDeviceConfig(ctx context.Context, apiURL string, insecure bool) (*DeviceConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiURL, "/")+"/api/v0_1/auth/keycloak/device-config", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device-config endpoint returned HTTP %d (SSO not configured?)", resp.StatusCode)
	}
	var cfg DeviceConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cannot parse device-config: %w", err)
	}
	if cfg.DeviceAuthorizationEndpoint == "" || cfg.TokenEndpoint == "" || cfg.ClientID == "" {
		return nil, errors.New("device-config response is missing required fields")
	}
	if err := validateEndpoint("device_authorization_endpoint", cfg.DeviceAuthorizationEndpoint, apiURL, insecure); err != nil {
		return nil, err
	}
	if err := validateEndpoint("token_endpoint", cfg.TokenEndpoint, apiURL, insecure); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateEndpoint ensures a discovered endpoint URL is acceptable:
//   - Refuses non-https schemes when !insecure.
//   - Refuses missing/invalid hosts.
//   - Warns to stderr (does NOT fail) when the endpoint host differs from
//     apiURL's host -- Keycloak is often on a separate subdomain, but a
//     completely unrelated host is worth surfacing in case a misconfigured
//     or malicious backend tries to steer the polling step elsewhere.
func validateEndpoint(field, endpointURL, apiURL string, insecure bool) error {
	u, err := url.Parse(endpointURL)
	if err != nil {
		return fmt.Errorf("device-config %s is not a valid URL: %w", field, err)
	}
	if u.Host == "" {
		return fmt.Errorf("device-config %s has no host: %q", field, endpointURL)
	}
	if !insecure && u.Scheme != "https" {
		return fmt.Errorf("device-config %s uses non-https scheme %q (pass --insecure to allow this)", field, u.Scheme)
	}
	if a, err := url.Parse(apiURL); err == nil && a.Host != "" && a.Host != u.Host {
		fmt.Fprintf(os.Stderr,
			"warning: device flow %s is at %q which differs from your API host %q.\n"+
				"         This is normal if Keycloak runs on a separate subdomain; refuse and re-run\n"+
				"         `dbg login` if you did not expect this.\n",
			field, u.Host, a.Host)
	}
	return nil
}

// LoginDevice runs the full device flow against the given backend URL.
// Stores tokens in the keychain on success and returns them. Honors ctx
// cancellation between polls and during HTTP calls.
func LoginDevice(ctx context.Context, apiURL string, insecure bool) (*Tokens, error) {
	cfg, err := DiscoverDeviceConfig(ctx, apiURL, insecure)
	if err != nil {
		return nil, err
	}

	dc, err := requestDeviceCode(ctx, cfg, insecure)
	if err != nil {
		return nil, err
	}

	displayURL := dc.VerificationURIComplete
	if displayURL == "" {
		displayURL = dc.VerificationURI
	}

	fmt.Printf("\n  To sign in, visit:    %s\n", displayURL)
	fmt.Printf("  Enter code:           %s\n\n", dc.UserCode)

	// Best-effort browser open; ignore failure (headless boxes are normal).
	_ = browser.OpenURL(displayURL)

	fmt.Print("  Waiting for approval...")
	tok, err := pollForToken(ctx, cfg, dc, insecure)
	if err != nil {
		fmt.Println(" failed")
		return nil, err
	}
	fmt.Println(" ✓")

	if err := StoreTokens(tok); err != nil {
		return nil, fmt.Errorf("cannot store tokens: %w", err)
	}
	return tok, nil
}

func requestDeviceCode(ctx context.Context, cfg *DeviceConfig, insecure bool) (*deviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	// scope is implicit in the Keycloak client config; if needed we'd add
	// `form.Set("scope", "openid profile email")` here.

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach device authorization endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorization failed (HTTP %d)", resp.StatusCode)
	}
	var dc deviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("cannot parse device code response: %w", err)
	}
	if dc.Interval == 0 {
		dc.Interval = 5 // RFC 8628 recommended default
	}
	return &dc, nil
}

// pollForToken polls the token endpoint at the device-code interval until
// the user approves, the device code expires, ctx is cancelled, or an
// unrecoverable error occurs. RFC 8628 reserves "authorization_pending"
// for keep-polling and "slow_down" for keep-polling-but-back-off.
func pollForToken(ctx context.Context, cfg *DeviceConfig, dc *deviceCodeResponse, insecure bool) (*Tokens, error) {
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	interval := time.Duration(dc.Interval) * time.Second

	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("device_code", dc.DeviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	for {
		if time.Now().After(deadline) {
			return nil, errors.New("device code expired before approval (try `dbg login` again)")
		}

		// Respect ctx cancellation between polls.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient(insecure).Do(req)
		if err != nil {
			fmt.Print(".")
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		var tr tokenResponse
		_ = json.Unmarshal(body, &tr)

		if resp.StatusCode == http.StatusOK && tr.AccessToken != "" {
			return &Tokens{
				AccessToken:  tr.AccessToken,
				RefreshToken: tr.RefreshToken,
				ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
				// Record where these Keycloak-issued tokens came from so the
				// refresh path renews them at Keycloak, not the backend.
				TokenEndpoint: cfg.TokenEndpoint,
				ClientID:      cfg.ClientID,
			}, nil
		}

		switch tr.Error {
		case "authorization_pending":
			fmt.Print(".")
			continue
		case "slow_down":
			interval += 5 * time.Second
			fmt.Print(".")
			continue
		case "access_denied":
			return nil, errors.New("authorization denied")
		case "expired_token":
			return nil, errors.New("device code expired before approval (try `dbg login` again)")
		}
		// Deliberately omit `body` from the error -- it may contain
		// tokens or other sensitive fields from a misbehaving IdP.
		return nil, fmt.Errorf("device flow failed (HTTP %d): %s", resp.StatusCode, firstNonEmpty(tr.ErrorDescription, tr.Error))
	}
}

// httpClient returns an HTTP client honouring the --insecure flag for
// self-signed dev backends. The CheckRedirect policy refuses to follow a
// redirect to a non-https URL when !insecure, preventing TLS downgrade
// via a malicious redirect.
func httpClient(insecure bool) *http.Client {
	c := &http.Client{
		Timeout: 30 * time.Second,
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
	if insecure {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	}
	return c
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
