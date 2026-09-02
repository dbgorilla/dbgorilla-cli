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
//     polling step. We do NOT enforce host-equality with apiURL, because the
//     identity provider is normally on a sibling subdomain -- but we warn
//     when an endpoint leaves the API's registrable domain entirely, so an
//     attacker cannot quietly redirect polling to a domain they control.
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

	"github.com/dbgorilla/dbgorilla-cli/internal/httpx"
	"github.com/pkg/browser"
	"golang.org/x/net/publicsuffix"
)

// openBrowser opens the verification URL in the user's default browser. It is
// a package-level seam so tests can substitute a no-op: calling the real
// browser.OpenURL during a test would launch `open`/`xdg-open` on the host,
// a non-hermetic side effect. Production behaviour is unchanged.
var openBrowser = browser.OpenURL

// pollUnit is the time unit applied to the device-code interval and expiry
// deadline. It is time.Second in production; tests override it (e.g. to a
// microsecond) so the polling loop runs to completion instantly. Keeping it a
// single knob means both the interval and the deadline scale together.
var pollUnit = time.Second

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

// ErrAPIUnreachable marks a device-config failure where the configured
// deployment never answered as itself -- a connection that did not land,
// something other than the API replying, or the API replying with an error of
// its own. Distinct from a deployment that answers and simply has no SSO,
// which is a 404 and a legitimate reason to fall back to password sign-in.
var ErrAPIUnreachable = errors.New("sign-in configuration unavailable")

// maxDeviceConfigBytes bounds how much of the response is read before giving
// up. The real document is a few hundred bytes; anything answering with a
// large body is not the endpoint we asked for.
const maxDeviceConfigBytes = 1 << 20

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
		// A refused cross-host redirect is a configured-URL problem, and its
		// message already says which host and how to point at it. Wrapping it
		// in "cannot reach ..." would bury that.
		var crossHost *httpx.CrossHostRedirectError
		if errors.As(err, &crossHost) {
			return nil, crossHost
		}
		return nil, fmt.Errorf("%w: cannot reach %s: %w", ErrAPIUnreachable, apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Only a 404 means "this deployment answered, and it has no SSO" -- the one
	// status that justifies dropping to a password prompt. Every other error
	// status is the deployment failing, not declining: a 502 from a proxy in
	// front of a dead backend, a 503 mid-deploy, a 401 from something guarding
	// the path. Treating those as "no SSO here" asks for credentials on behalf
	// of a deployment that is not currently working, and throws away the status
	// code that would have explained it.
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("device-config endpoint returned HTTP 404 (SSO not configured?)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned HTTP %d", ErrAPIUnreachable, apiURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDeviceConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("cannot read device-config response: %w", err)
	}
	// A 200 carrying a web page means something other than the API answered --
	// a login portal, an SPA catch-all, or a proxy. Say that, rather than
	// letting the JSON decoder report a stray "<" the user cannot act on.
	if httpx.IsHTML(body) {
		return nil, fmt.Errorf(
			"%w: %s answered with a web page.\n"+
				"  Something other than the DBGorilla API is answering this address --\n"+
				"  commonly a login portal, a proxy, or a deployment that has moved.\n"+
				"  Check where the CLI is pointed: dbg config get api-url",
			ErrAPIUnreachable, apiURL)
	}
	var cfg DeviceConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse device-config: %w", err)
	}
	if cfg.DeviceAuthorizationEndpoint == "" || cfg.TokenEndpoint == "" || cfg.ClientID == "" {
		return nil, errors.New("device-config response is missing required fields")
	}
	authHost, err := validateEndpoint("device_authorization_endpoint", cfg.DeviceAuthorizationEndpoint, insecure)
	if err != nil {
		return nil, err
	}
	tokenHost, err := validateEndpoint("token_endpoint", cfg.TokenEndpoint, insecure)
	if err != nil {
		return nil, err
	}
	warnOffDomainEndpoints(os.Stderr, apiURL, authHost, tokenHost)
	return &cfg, nil
}

// validateEndpoint ensures a discovered endpoint URL is acceptable and returns
// its hostname (no port):
//   - Refuses non-https schemes when !insecure.
//   - Refuses missing/invalid hosts.
//
// Whether the host is a reasonable one to be sent to is a separate question,
// answered once for both endpoints by warnOffDomainEndpoints.
func validateEndpoint(field, endpointURL string, insecure bool) (string, error) {
	u, err := url.Parse(endpointURL)
	if err != nil {
		return "", fmt.Errorf("device-config %s is not a valid URL: %w", field, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("device-config %s has no host: %q", field, endpointURL)
	}
	if !insecure && u.Scheme != "https" {
		return "", fmt.Errorf("device-config %s uses non-https scheme %q (pass --insecure to allow this)", field, u.Scheme)
	}
	return u.Hostname(), nil
}

// warnOffDomainEndpoints warns when sign-in would be handed to a host outside
// the API's own registrable domain.
//
// The warning exists for one case: a backend -- misconfigured, or hostile --
// pointing the token-polling step at a host someone else controls. It is not
// for the ordinary case, which is the identity provider sitting on a sibling
// subdomain of the same company domain. Warning on any host difference at all
// meant the ordinary case printed two alarming paragraphs on the first command
// a new user ever runs, which teaches people to skip the text. So:
//
//   - Same registrable domain (auth.example.com vs api.example.com): silent.
//   - Different registrable domain (idp.attacker.net vs api.example.com):
//     one line, once per distinct host, naming the host and what to do.
//
// "Registrable domain" and not "last two labels", because the shortcut is
// wrong precisely where it matters: under a multi-label public suffix,
// attacker.co.uk and yourcompany.co.uk share their last two labels, and the
// shortcut would stay silent for the exact handover this warning is for.
func warnOffDomainEndpoints(w io.Writer, apiURL string, endpointHosts ...string) {
	a, err := url.Parse(apiURL)
	if err != nil || a.Hostname() == "" {
		return
	}
	apiHost := a.Hostname()
	warned := make(map[string]bool, len(endpointHosts))
	for _, host := range endpointHosts {
		if host == "" || warned[host] || sameRegistrableDomain(host, apiHost) {
			continue
		}
		warned[host] = true
		_, _ = fmt.Fprintf(w, "warning: sign-in is handled by %s, which is outside %s -- press Ctrl-C now if that is not your identity provider.\n",
			host, describeDomain(apiHost))
	}
}

// sameRegistrableDomain reports whether two hostnames belong to the same
// registrable domain -- the level at which one party controls the name.
//
// Hosts with no registrable domain (a bare name like "localhost", an IP
// literal, a name that is itself a public suffix) have no such party to
// compare, so only exact equality counts. Failing closed here is deliberate:
// an unknown pair gets the warning rather than silence.
func sameRegistrableDomain(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	da, errA := publicsuffix.EffectiveTLDPlusOne(a)
	db, errB := publicsuffix.EffectiveTLDPlusOne(b)
	if errA != nil || errB != nil {
		return false
	}
	return da == db
}

// describeDomain names the boundary the warning is about: the API's
// registrable domain when there is one, and the bare host otherwise (a
// development box on "localhost", say, where there is nothing shorter to say).
func describeDomain(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(host)); err == nil {
		return d
	}
	return host
}

// LoginDevice runs the full device flow against the given backend URL,
// discovering the device-config itself. Callers that have already discovered
// it (to auto-detect the login mode, say) should use LoginDeviceWithConfig so
// the endpoint checks are not run -- and their warnings not printed -- twice.
func LoginDevice(ctx context.Context, apiURL string, insecure bool) (*Tokens, error) {
	cfg, err := DiscoverDeviceConfig(ctx, apiURL, insecure)
	if err != nil {
		return nil, err
	}
	return LoginDeviceWithConfig(ctx, cfg, insecure)
}

// LoginDeviceWithConfig runs the device flow against an already-discovered
// config. Stores tokens in the keychain on success and returns them. Honors
// ctx cancellation between polls and during HTTP calls.
func LoginDeviceWithConfig(ctx context.Context, cfg *DeviceConfig, insecure bool) (*Tokens, error) {
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
	_ = openBrowser(displayURL)

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
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * pollUnit)
	interval := time.Duration(dc.Interval) * pollUnit

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
			interval += 5 * pollUnit
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
// self-signed dev backends. Redirect handling is httpx.RedirectPolicy, shared
// with the API client: no TLS downgrade, no endless chain, and no leaving the
// host the request was aimed at.
func httpClient(insecure bool) *http.Client {
	c := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: httpx.RedirectPolicy(insecure),
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
