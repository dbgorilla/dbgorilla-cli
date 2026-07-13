package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// withStdin replaces os.Stdin with a temp file holding content and restores
// it afterwards. A regular file is not a tty, so readPassword takes its
// non-interactive line-read branch (no pty needed).
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		_ = f.Close()
	})
}

// withBlockingStdin points os.Stdin at the read end of a pipe whose write end
// is never written to or closed during the test -- so a read against it
// blocks indefinitely, exactly like a real interactive prompt waiting on a
// human. Used to exercise ctx cancellation instead of a real read completing.
func withBlockingStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = w.Close()
		_ = r.Close()
	})
}

// --- PromptCredentials -----------------------------------------------------

func TestPromptCredentials_AllPrefilledReadsNothing(t *testing.T) {
	// stdin left as-is: if the code tried to read, an unexpected read would
	// still succeed against the inherited fd, so guard by using empty stdin.
	withStdin(t, "")
	in := PasswordCredentials{Tenant: "acme", Account: "sysop", Password: "hunter2"}
	got, err := PromptCredentials(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != in {
		t.Errorf("got %+v, want unchanged %+v", got, in)
	}
}

func TestPromptCredentials_ReadsTenantAndAccountFromStdin(t *testing.T) {
	withStdin(t, "acme\nsysop\n")
	got, err := PromptCredentials(context.Background(), PasswordCredentials{Password: "pw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tenant != "acme" || got.Account != "sysop" || got.Password != "pw" {
		t.Errorf("got %+v", got)
	}
}

func TestPromptCredentials_ReadsPasswordFromStdin(t *testing.T) {
	// Only the password is read here, so a single reader touches stdin.
	withStdin(t, "s3cret\n")
	got, err := PromptCredentials(context.Background(), PasswordCredentials{Tenant: "acme", Account: "sysop"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Password != "s3cret" {
		t.Errorf("password = %q, want s3cret", got.Password)
	}
}

func TestPromptCredentials_MissingFieldsError(t *testing.T) {
	// Password prefilled (so readPassword is skipped), but tenant/account read
	// back empty from blank lines -> the "all required" validation fires.
	withStdin(t, "\n\n")
	_, err := PromptCredentials(context.Background(), PasswordCredentials{Password: "pw"})
	if err == nil || !strings.Contains(err.Error(), "all required") {
		t.Fatalf("err = %v, want 'all required'", err)
	}
}

func TestPromptCredentials_EmptyStdinAllFields(t *testing.T) {
	// Nothing prefilled and empty stdin: the tenant prompt hits EOF (returns
	// ""), and the flow ultimately errors out.
	withStdin(t, "")
	if _, err := PromptCredentials(context.Background(), PasswordCredentials{}); err == nil {
		t.Fatal("expected error with no input available")
	}
}

func TestPromptCredentials_PasswordReadErrorPropagates(t *testing.T) {
	// Tenant/account prefilled; empty stdin -> readPassword hits EOF and the
	// error must propagate out of PromptCredentials.
	withStdin(t, "")
	_, err := PromptCredentials(context.Background(), PasswordCredentials{Tenant: "acme", Account: "sysop"})
	if err == nil {
		t.Fatal("expected error when password read hits EOF, got nil")
	}
}

// TestPromptCredentials_CtxCancelDuringPrompt pins the actual bug fix: Ctrl-C
// (modeled here as ctx cancellation, exactly as `dbg login` wires SIGINT via
// signal.NotifyContext) must abort a blocked prompt promptly instead of the
// blocking stdin read silently swallowing it forever.
func TestPromptCredentials_CtxCancelDuringPrompt(t *testing.T) {
	withBlockingStdin(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := PromptCredentials(ctx, PasswordCredentials{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("PromptCredentials took %v to return after cancellation, want prompt return", elapsed)
	}
}

// --- LoginPassword ---------------------------------------------------------

func TestLoginPassword_Success(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()

	var gotBody map[string]string
	var gotContentType, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0_1/auth/token" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at",
			"refresh_token": "rt",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	creds := PasswordCredentials{Tenant: "acme", Account: "sysop", Password: "pw"}
	tok, err := LoginPassword(srv.URL, true, creds)
	if err != nil {
		t.Fatalf("LoginPassword: %v", err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Errorf("tokens = %+v", tok)
	}
	if tok.IsExpired() {
		t.Error("token with 3600s expiry should not be expired")
	}
	// Request shape.
	if gotContentType != "application/json" || gotAccept != "application/json" {
		t.Errorf("headers content-type=%q accept=%q", gotContentType, gotAccept)
	}
	if gotBody["account"] != "sysop" || gotBody["password"] != "pw" ||
		gotBody["tenant"] != "acme" || gotBody["account_type"] != "USERNAME" {
		t.Errorf("request body = %+v", gotBody)
	}
	// Tokens were persisted.
	stored, err := LoadTokens()
	if err != nil || stored.AccessToken != "at" {
		t.Errorf("stored=%+v err=%v", stored, err)
	}
}

func TestLoginPassword_DefaultsExpiryWhenMissing(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"}) // no expires_in
	}))
	defer srv.Close()

	tok, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	// Defaulted to ~1h out.
	if got := time.Until(tok.ExpiresAt); got < 59*time.Minute || got > 61*time.Minute {
		t.Errorf("default expiry = %v, want ~1h", got)
	}
}

func TestLoginPassword_AuthFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
			if err == nil || !strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("err = %v, want authentication failed", err)
			}
		})
	}
}

func TestLoginPassword_ErrorWithDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "backend on fire"})
	}))
	defer srv.Close()
	_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "backend on fire") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want detail + status", err)
	}
}

func TestLoginPassword_ErrorWithoutDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "login failed (HTTP 422)") {
		t.Fatalf("err = %v, want generic status error", err)
	}
}

func TestLoginPassword_MalformedSuccessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{ not json"))
	}))
	defer srv.Close()
	_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "cannot parse token response") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

func TestLoginPassword_MissingAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refresh_token": "rt"}) // no access_token
	}))
	defer srv.Close()
	_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("err = %v, want missing access_token", err)
	}
}

func TestLoginPassword_StoreFails(t *testing.T) {
	// Config dir unresolvable (XDG points at a file) + broken keychain means a
	// successful HTTP login still can't persist -> "cannot store tokens".
	fileAsBase := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileAsBase, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", fileAsBase)
	t.Setenv("HOME", fileAsBase)
	keyring.MockInitWithError(errors.New("no keychain"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600})
	}))
	defer srv.Close()

	_, err := LoginPassword(srv.URL, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "cannot store tokens") {
		t.Fatalf("err = %v, want store failure", err)
	}
}

func TestLoginPassword_BadRequestURL(t *testing.T) {
	// A control character in the API URL fails request construction.
	_, err := LoginPassword("http://bad\n", true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil {
		t.Fatal("expected request-build error for control-char API URL")
	}
}

func TestLoginPassword_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening

	_, err := LoginPassword(url, true, PasswordCredentials{Tenant: "t", Account: "a", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("err = %v, want cannot reach", err)
	}
}
