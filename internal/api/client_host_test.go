package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/zalando/go-keyring"
)

func isolateKeychain(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
}

// The stored session must never reach a host other than the one it was
// issued for, whatever --api-url says.
func TestClientRefusesTokensForAnotherHost(t *testing.T) {
	isolateKeychain(t)
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := func(apiURL string) {
		t.Helper()
		if err := auth.StoreTokens(&auth.Tokens{AccessToken: "secret", ExpiresAt: time.Now().Add(time.Hour), APIURL: apiURL}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("issued for another host: no request, clear error", func(t *testing.T) {
		gotAuth = nil
		store("https://elsewhere.example")
		_, _, err := NewClient(srv.URL).Get("/whoami")
		if err == nil || !strings.Contains(err.Error(), "signed in to elsewhere.example") || !strings.Contains(err.Error(), "dbgorilla login --api-url") {
			t.Fatalf("want a refusal naming both hosts and the fix, got err=%v", err)
		}
		if len(gotAuth) != 0 {
			t.Fatalf("no request may leave the client, got %d", len(gotAuth))
		}
	})

	t.Run("issued for this host: token sent", func(t *testing.T) {
		gotAuth = nil
		store(srv.URL)
		if _, _, err := NewClient(srv.URL).Get("/whoami"); err != nil {
			t.Fatal(err)
		}
		if len(gotAuth) != 1 || gotAuth[0] != "Bearer secret" {
			t.Fatalf("want the token on the request, got %v", gotAuth)
		}
	})

	t.Run("session stored before hosts were recorded: still sent", func(t *testing.T) {
		gotAuth = nil
		store("")
		if _, _, err := NewClient(srv.URL).Get("/whoami"); err != nil {
			t.Fatal(err)
		}
		if len(gotAuth) != 1 || gotAuth[0] != "Bearer secret" {
			t.Fatalf("legacy session must keep working, got %v", gotAuth)
		}
	})
}
