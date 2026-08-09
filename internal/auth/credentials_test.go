package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// isolatedConfigDir points config.Dir() at a throwaway directory so the
// fallback credentials file lands under t.TempDir() and never touches the
// developer's real ~/.config. Returns the config base (the credentials file
// lives at <base>/dbgorilla/credentials.json).
func isolatedConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// HOME is the fallback base if XDG were empty; pin it too for hermeticity.
	t.Setenv("HOME", dir)
	return dir
}

func sampleTokens() *Tokens {
	return &Tokens{
		AccessToken:   "access-abc",
		RefreshToken:  "refresh-xyz",
		ExpiresAt:     time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		TokenEndpoint: "https://idp.example/token",
		ClientID:      "dbgorilla-cli",
	}
}

func assertTokensEqual(t *testing.T, got, want *Tokens) {
	t.Helper()
	if got.AccessToken != want.AccessToken ||
		got.RefreshToken != want.RefreshToken ||
		got.TokenEndpoint != want.TokenEndpoint ||
		got.ClientID != want.ClientID ||
		!got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("tokens differ:\n got=%+v\nwant=%+v", got, want)
	}
}

// --- IsExpired (pure math) -------------------------------------------------

func TestTokens_IsExpired(t *testing.T) {
	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"already past", time.Now().Add(-time.Hour), true},
		{"inside the 60s safety buffer", time.Now().Add(30 * time.Second), true},
		{"comfortably in the future", time.Now().Add(2 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := &Tokens{ExpiresAt: tc.expiresAt}
			if got := tok.IsExpired(); got != tc.want {
				t.Errorf("IsExpired() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- keychain round-trip ---------------------------------------------------

func TestStoreLoadTokens_KeychainRoundTrip(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()

	// Pre-seed a stale fallback file; a working keychain store must delete it.
	stale := fallbackPath()
	if err := os.WriteFile(stale, []byte(`{"access_token":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}

	want := sampleTokens()
	if err := StoreTokens(want); err != nil {
		t.Fatalf("StoreTokens: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("fallback file should have been removed after keychain success (stat err=%v)", err)
	}

	got, err := LoadTokens()
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	assertTokensEqual(t, got, want)
}

func TestLoadTokens_MalformedKeychainData(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()
	if err := keyring.Set(keyringService, keyringKey, "{ not valid json"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokens(); err == nil || !strings.Contains(err.Error(), "cannot parse stored tokens") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

// --- fallback file path (keychain unavailable) -----------------------------

func TestStoreTokens_FallsBackToFile(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInitWithError(errors.New("keychain is locked"))

	want := sampleTokens()
	if err := StoreTokens(want); err != nil {
		t.Fatalf("StoreTokens: %v", err)
	}

	path := fallbackPath()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected fallback file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("fallback file perms = %o, want 0600", perm)
	}

	// Keychain still broken -> LoadTokens must read the file back.
	got, err := LoadTokens()
	if err != nil {
		t.Fatalf("LoadTokens (fallback): %v", err)
	}
	assertTokensEqual(t, got, want)
}

func TestStoreTokens_OverwritesWeakPermFallbackFile(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInitWithError(errors.New("no keychain"))

	// Simulate a pre-existing world-readable file; storeFallbackFile must
	// Chmod it back to 0600 rather than preserve the weak mode.
	path := fallbackPath()
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}

	if err := StoreTokens(sampleTokens()); err != nil {
		t.Fatalf("StoreTokens: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("perms after overwrite = %o, want 0600", perm)
	}
}

func TestLoadTokens_NothingStored(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit() // empty store -> Get returns ErrNotFound -> fallback
	if _, err := LoadTokens(); err == nil || !strings.Contains(err.Error(), "no stored credentials") {
		t.Fatalf("err = %v, want 'no stored credentials'", err)
	}
}

func TestLoadFallbackFile_Malformed(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInitWithError(keyring.ErrNotFound)
	if err := os.WriteFile(fallbackPath(), []byte("{ broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokens(); err == nil || !strings.Contains(err.Error(), "cannot parse stored credentials") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

// --- ClearTokens -----------------------------------------------------------

func TestClearTokens_RemovesKeychainAndFile(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()

	if err := StoreTokens(sampleTokens()); err != nil {
		t.Fatal(err)
	}
	// Also drop a fallback file so we exercise removing both.
	if err := os.WriteFile(fallbackPath(), []byte(`{"access_token":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := ClearTokens(); err != nil {
		t.Fatalf("ClearTokens: %v", err)
	}
	if _, err := keyring.Get(keyringService, keyringKey); err == nil {
		t.Error("keychain entry should be gone after ClearTokens")
	}
	if _, err := os.Stat(fallbackPath()); !os.IsNotExist(err) {
		t.Errorf("fallback file should be gone, stat err=%v", err)
	}
}

// --- config dir unavailable ------------------------------------------------

// When config.Dir() cannot be resolved (here: XDG_CONFIG_HOME points at a
// regular file so MkdirAll fails), fallbackPath() returns "" and both the
// store and load fallback helpers must surface a clean error rather than
// panic or write to a bogus path.
func TestFallbackFile_ConfigDirUnavailable(t *testing.T) {
	fileAsBase := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(fileAsBase, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", fileAsBase)
	t.Setenv("HOME", fileAsBase)

	if p := fallbackPath(); p != "" {
		t.Fatalf("fallbackPath() = %q, want empty when config dir unresolvable", p)
	}

	keyring.MockInitWithError(errors.New("no keychain"))
	if err := StoreTokens(sampleTokens()); err == nil || !strings.Contains(err.Error(), "cannot determine config directory") {
		t.Fatalf("StoreTokens err = %v, want config-dir error", err)
	}

	keyring.MockInitWithError(keyring.ErrNotFound)
	if _, err := LoadTokens(); err == nil || !strings.Contains(err.Error(), "no stored credentials") {
		t.Fatalf("LoadTokens err = %v, want no-credentials error", err)
	}
}

// If the credentials path can't be written (here: it already exists as a
// directory), storeFallbackFile must return the underlying write error.
func TestStoreFallbackFile_WriteError(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInitWithError(errors.New("no keychain"))
	if err := os.MkdirAll(fallbackPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := StoreTokens(sampleTokens()); err == nil {
		t.Fatal("expected write error when credentials path is a directory")
	}
}
