package auth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/zalando/go-keyring"
)

const (
	keyringService = "dbgorilla"
	keyringKey     = "tokens"
)

// Tokens holds the OAuth token pair.
//
// TokenEndpoint and ClientID are set only for device/SSO-flow sessions, whose
// tokens are issued directly by Keycloak. Their presence tells the refresh
// logic to renew at the Keycloak token endpoint (an OAuth refresh_token grant)
// rather than the DBGorilla backend's /token/refresh endpoint, which only
// validates backend-issued (password-flow) tokens. When they are empty
// (password flow) the backend refresh path is used.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	// TokenEndpoint is the Keycloak token endpoint for SSO-flow refresh.
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	// ClientID is the OAuth client used for the SSO-flow refresh grant.
	ClientID string `json:"client_id,omitempty"`
	// APIURL is the deployment these tokens were issued for. A request to any
	// other host must not carry them: --api-url is user input, and a token
	// sent to the wrong host is a token handed to whoever runs that host.
	// Empty for sessions stored before this field existed.
	APIURL string `json:"api_url,omitempty"`
}

// IssuedFor reports whether the tokens may be sent to apiURL. A session that
// predates APIURL (empty) is accepted, so an upgrade does not sign everyone
// out; the next login records the host.
func (t *Tokens) IssuedFor(apiURL string) bool {
	if t.APIURL == "" {
		return true
	}
	return HostOf(t.APIURL) == HostOf(apiURL)
}

// HostOf returns the lower-cased host (with port, if any) of a URL, or the
// input itself when it does not parse as one.
func HostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(rawURL))
	}
	return strings.ToLower(u.Host)
}

// IsExpired returns true if the access token has expired (with 60s buffer).
func (t *Tokens) IsExpired() bool {
	return time.Now().After(t.ExpiresAt.Add(-60 * time.Second))
}

// StoreTokens persists tokens to the OS keychain. Falls back to a local file
// with 0600 permissions if the keychain is unavailable, and prints a warning.
func StoreTokens(tokens *Tokens) error {
	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("cannot serialize tokens: %w", err)
	}

	err = keyring.Set(keyringService, keyringKey, string(data))
	if err == nil {
		// Also remove any fallback file if keychain works
		removeFallbackFile()
		return nil
	}

	// Keychain unavailable -- fall back to file
	fmt.Fprintln(os.Stderr, "Warning: OS keychain unavailable. Storing tokens in plaintext file.")
	return storeFallbackFile(data)
}

// LoadTokens reads tokens from keychain or fallback file.
func LoadTokens() (*Tokens, error) {
	data, err := keyring.Get(keyringService, keyringKey)
	if err == nil {
		var t Tokens
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return nil, fmt.Errorf("cannot parse stored tokens: %w", err)
		}
		return &t, nil
	}

	// Try fallback file
	return loadFallbackFile()
}

// ClearTokens removes tokens from keychain and fallback file.
func ClearTokens() error {
	_ = keyring.Delete(keyringService, keyringKey)
	removeFallbackFile()
	return nil
}

// --- fallback file helpers ---

func fallbackPath() string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "credentials.json")
}

func storeFallbackFile(data []byte) error {
	path := fallbackPath()
	if path == "" {
		return fmt.Errorf("cannot determine config directory for fallback credentials")
	}
	// os.WriteFile only applies the mode on file creation. If the file
	// already exists with weaker perms (e.g. from a previous tool or a
	// backup-restored layout) the write preserves them. Explicit Chmod
	// closes that gap.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func loadFallbackFile() (*Tokens, error) {
	path := fallbackPath()
	if path == "" {
		return nil, fmt.Errorf("no stored credentials found")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no stored credentials found")
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("cannot parse stored credentials: %w", err)
	}
	return &t, nil
}

func removeFallbackFile() {
	path := fallbackPath()
	if path != "" {
		_ = os.Remove(path)
	}
}
