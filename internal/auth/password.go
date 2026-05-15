// Internal username/password login against POST /api/v0_1/auth/token.
//
// Used when the backend has AUTH_PROVIDER=internal (no Keycloak), or when
// the user explicitly forces password mode via --mode password. The CLI
// never accepts the password as a flag -- it's read from stdin without
// echo so it doesn't land in shell history or process listings.
package auth

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// PasswordCredentials are the inputs collected from the user.
type PasswordCredentials struct {
	Tenant   string
	Account  string
	Password string
}

// PromptCredentials reads any missing fields from stdin. Tenant/account are
// echoed; password is hidden when stdin is a tty.
func PromptCredentials(prefill PasswordCredentials) (PasswordCredentials, error) {
	creds := prefill
	r := bufio.NewReader(os.Stdin)

	if creds.Tenant == "" {
		creds.Tenant = promptLine(r, "Tenant")
	}
	if creds.Account == "" {
		creds.Account = promptLine(r, "Account")
	}
	if creds.Password == "" {
		pw, err := readPassword("Password: ")
		if err != nil {
			return creds, err
		}
		creds.Password = pw
	}
	if creds.Tenant == "" || creds.Account == "" || creds.Password == "" {
		return creds, errors.New("tenant, account, and password are all required")
	}
	return creds, nil
}

// LoginPassword exchanges credentials for tokens and stores them.
// The backend distinguishes USERNAME from EMAIL login via account_type.
// We default to USERNAME because that is what every documented dev account
// (sysop, debug-user, integration-test users) uses; EMAIL is a less common
// path the user can request explicitly later if needed.
func LoginPassword(apiURL string, insecure bool, creds PasswordCredentials) (*Tokens, error) {
	body, _ := json.Marshal(map[string]string{
		"account":      creds.Account,
		"password":     creds.Password,
		"tenant":       creds.Tenant,
		"account_type": "USERNAME",
	})
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(apiURL, "/")+"/api/v0_1/auth/token",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("authentication failed (check tenant/account/password)")
	}
	if resp.StatusCode != http.StatusOK {
		// Echo only the FastAPI-style `detail` field if present. Returning
		// the raw response body would risk leaking submitted credentials --
		// FastAPI 422 validation responses echo the input fields by default.
		var errResp struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		if errResp.Detail != "" {
			return nil, fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, errResp.Detail)
		}
		return nil, fmt.Errorf("login failed (HTTP %d)", resp.StatusCode)
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return nil, fmt.Errorf("cannot parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}

	exp := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		// If backend doesn't tell us, assume one hour. Refresh handles real expiry.
		exp = time.Now().Add(time.Hour)
	}

	tok := &Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    exp,
	}
	if err := StoreTokens(tok); err != nil {
		return nil, fmt.Errorf("cannot store tokens: %w", err)
	}
	return tok, nil
}

// --- prompt helpers --------------------------------------------------------

func promptLine(r *bufio.Reader, label string) string {
	fmt.Fprintf(os.Stderr, "  %s: ", label)
	line, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

func readPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "  %s", prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-tty: read one line. Caller accepts that scripted invocations
	// may put the password on stdin without echo control.
	r := bufio.NewReader(os.Stdin)
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\n"), nil
}
