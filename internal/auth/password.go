// Internal username/password login against POST /api/v0_1/auth/token.
//
// Used when the backend has AUTH_PROVIDER=internal (no Keycloak), or when
// the user explicitly forces password mode via --mode password. The CLI
// never accepts the password as a flag -- it's read from stdin without
// echo so it doesn't land in shell history or process listings.
package auth

import (
	"bufio"
	"context"
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
// echoed; password is hidden when stdin is a tty. ctx cancellation (e.g.
// Ctrl-C, which `dbg login` wires to ctx via signal.NotifyContext) aborts
// whichever prompt is in flight instead of leaving the blocking stdin read
// to swallow the interrupt silently.
func PromptCredentials(ctx context.Context, prefill PasswordCredentials) (PasswordCredentials, error) {
	creds := prefill
	// One reader for every prompt. Each prompt used to open its own, and the
	// first one buffered all of piped stdin, so the password read hit EOF.
	r := bufio.NewReader(os.Stdin)
	if creds.Tenant == "" {
		v, err := promptLine(ctx, r, "Organization name (as on the sign-up form)")
		if err != nil {
			return creds, err
		}
		creds.Tenant = v
	}
	if creds.Account == "" {
		v, err := promptLine(ctx, r, "Username or email")
		if err != nil {
			return creds, err
		}
		creds.Account = v
	}
	if creds.Password == "" {
		pw, err := readPassword(ctx, r, "Password: ")
		if err != nil {
			return creds, err
		}
		creds.Password = pw
	}
	var missing []string
	if creds.Tenant == "" {
		missing = append(missing, "organization name")
	}
	if creds.Account == "" {
		missing = append(missing, "username or email")
	}
	if creds.Password == "" {
		missing = append(missing, "password")
	}
	if len(missing) > 0 {
		msg := "missing " + strings.Join(missing, ", ")
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			msg += noKeyboardHint()
		}
		return creds, errors.New(msg)
	}
	return creds, nil
}

// noKeyboardHint explains what to do when there is no terminal to type into,
// for example inside a coding agent.
func noKeyboardHint() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "dbgorilla"
	}
	return ".\n  There is no keyboard to read from here. Either run this in a separate terminal:\n" +
		"    " + exe + " login --mode password\n" +
		"  or pass --organization and --account and pipe the password on standard input"
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
		return nil, loginRefused(respBody, creds)
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
		APIURL:       apiURL,
	}
	if err := StoreTokens(tok); err != nil {
		return nil, fmt.Errorf("cannot store tokens: %w", err)
	}
	return tok, nil
}

// loginRefused turns a 401/403 into a message that says which field to check.
// The server names the field when it can (an unknown organization); otherwise
// the username and the password cannot be told apart without revealing which
// accounts exist, so the message says so.
func loginRefused(body []byte, creds PasswordCredentials) error {
	var errResp struct {
		Detail json.RawMessage `json:"detail"`
	}
	_ = json.Unmarshal(body, &errResp)
	var text string
	var obj struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(errResp.Detail, &text) != nil {
		_ = json.Unmarshal(errResp.Detail, &obj)
	}
	d := strings.ToLower(text)
	switch {
	case strings.Contains(obj.Code, "verification") || strings.Contains(d, "verif"):
		return errors.New("sign-in failed: the email address on this account is not verified yet. " +
			"Click the link in the verification email, then sign in again")
	case strings.Contains(d, "organization"):
		return fmt.Errorf("sign-in failed: no organization named %q. Use the organization name exactly as you typed it on the sign-up form", creds.Tenant)
	case text != "" && !strings.Contains(d, "incorrect") && !strings.Contains(d, "invalid"):
		return fmt.Errorf("sign-in failed: %s", text)
	}
	return fmt.Errorf("sign-in failed: the organization name, the username or email, or the password is wrong (organization %q).\n"+
		"  The organization name must match the sign-up form exactly.\n"+
		"  Signed up with Google, GitHub or GitLab? Run `dbgorilla login` without --mode password", creds.Tenant)
}

// --- prompt helpers --------------------------------------------------------
//
// A plain bufio/term read has no way to observe context cancellation -- it
// just blocks on the fd. Ctrl-C during one of these prompts used to do
// nothing at all: `dbg login` installs its own SIGINT handler for the whole
// command (via signal.NotifyContext, to interrupt the SSO device-flow poll
// loop), which disables the OS's default kill-on-SIGINT behavior, but these
// prompts never checked ctx.Done() -- so the signal was captured and then
// dropped on the floor. Each helper below runs its blocking read on a
// goroutine and races it against ctx.Done() so cancellation actually aborts
// the prompt.

// readLineCtx reads one line from r, or returns ctx.Err() (cancelled=true) if
// ctx is cancelled first. The read goroutine is left running in that case --
// harmless, since the process exits shortly after RunE returns an error.
func readLineCtx(ctx context.Context, r *bufio.Reader) (line string, err error, cancelled bool) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, e := r.ReadString('\n')
		ch <- result{l, e}
	}()
	select {
	case res := <-ch:
		return res.line, res.err, false
	case <-ctx.Done():
		return "", ctx.Err(), true
	}
}

func promptLine(ctx context.Context, r *bufio.Reader, label string) (string, error) {
	fmt.Fprintf(os.Stderr, "  %s: ", label)
	line, err, cancelled := readLineCtx(ctx, r)
	if cancelled {
		fmt.Fprintln(os.Stderr)
		return "", err
	}
	if err != nil {
		// Unreadable stdin (EOF etc.): treat as blank, matching prior
		// behavior. Downstream validation in PromptCredentials catches a
		// still-missing field.
		return "", nil
	}
	return strings.TrimSpace(line), nil
}

func readPassword(ctx context.Context, r *bufio.Reader, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "  %s", prompt)
		oldState, stateErr := term.GetState(fd)

		type result struct {
			pw  []byte
			err error
		}
		ch := make(chan result, 1)
		go func() {
			b, err := term.ReadPassword(fd)
			ch <- result{b, err}
		}()

		select {
		case res := <-ch:
			fmt.Fprintln(os.Stderr)
			if res.err != nil {
				return "", res.err
			}
			return string(res.pw), nil
		case <-ctx.Done():
			// term.ReadPassword is still blocked in the goroutine above and
			// its own deferred termios restore never runs. Restore echo
			// ourselves so Ctrl-C doesn't leave the shell silently
			// non-echoing until the user runs `reset`/`stty echo`.
			if stateErr == nil {
				_ = term.Restore(fd, oldState)
			}
			fmt.Fprintln(os.Stderr)
			return "", ctx.Err()
		}
	}
	// Non-tty: read one line. Caller accepts that scripted invocations
	// may put the password on stdin without echo control.
	line, err, cancelled := readLineCtx(ctx, r)
	if cancelled {
		return "", err
	}
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		// Nothing to read: no blank password is sent; the caller explains.
		return "", nil
	}
	return strings.TrimRight(line, "\r\n"), nil
}
