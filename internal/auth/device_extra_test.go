package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// stubBrowser replaces the openBrowser seam with a recorder so tests never
// launch a real browser. Returns a pointer to the last URL passed.
func stubBrowser(t *testing.T) *string {
	t.Helper()
	var last string
	old := openBrowser
	openBrowser = func(u string) error {
		last = u
		return nil
	}
	t.Cleanup(func() { openBrowser = old })
	return &last
}

// fastPoll shrinks the poll time unit so the device flow runs instantly.
func fastPoll(t *testing.T) {
	t.Helper()
	old := pollUnit
	pollUnit = time.Millisecond
	t.Cleanup(func() { pollUnit = old })
}

// --- IsDeviceFlowAvailable -------------------------------------------------

func TestIsDeviceFlowAvailable(t *testing.T) {
	valid := DeviceConfig{
		DeviceAuthorizationEndpoint: "https://idp.example/device",
		TokenEndpoint:               "https://idp.example/token",
		ClientID:                    "dbgorilla-cli",
	}
	t.Run("available", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(valid)
		}))
		defer srv.Close()
		if !IsDeviceFlowAvailable(context.Background(), srv.URL, false) {
			t.Error("want available=true")
		}
	})
	t.Run("not configured (404)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		if IsDeviceFlowAvailable(context.Background(), srv.URL, false) {
			t.Error("want available=false on 404")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		if IsDeviceFlowAvailable(context.Background(), url, true) {
			t.Error("want available=false when unreachable")
		}
	})
}

// --- DiscoverDeviceConfig error branches -----------------------------------

func TestDiscoverDeviceConfig_ErrorBranches(t *testing.T) {
	t.Run("non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		_, err := DiscoverDeviceConfig(context.Background(), srv.URL, false)
		if err == nil || !strings.Contains(err.Error(), "SSO not configured") {
			t.Fatalf("err = %v, want status error", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{ not json"))
		}))
		defer srv.Close()
		_, err := DiscoverDeviceConfig(context.Background(), srv.URL, false)
		if err == nil || !strings.Contains(err.Error(), "cannot parse device-config") {
			t.Fatalf("err = %v, want parse error", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := DiscoverDeviceConfig(context.Background(), url, true)
		if err == nil || !strings.Contains(err.Error(), "cannot reach") {
			t.Fatalf("err = %v, want cannot reach", err)
		}
	})
	t.Run("invalid device_authorization_endpoint url", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(DeviceConfig{
				DeviceAuthorizationEndpoint: "https://%zz-bad-escape/device",
				TokenEndpoint:               "https://idp/token",
				ClientID:                    "x",
			})
		}))
		defer srv.Close()
		_, err := DiscoverDeviceConfig(context.Background(), srv.URL, false)
		if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
			t.Fatalf("err = %v, want URL parse error", err)
		}
	})
	t.Run("invalid token_endpoint url", func(t *testing.T) {
		// device_authorization_endpoint is fine; the token_endpoint is not, so
		// the second validateEndpoint call is what rejects it.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(DeviceConfig{
				DeviceAuthorizationEndpoint: "https://idp.example/device",
				TokenEndpoint:               "http://idp.example/token", // non-https
				ClientID:                    "x",
			})
		}))
		defer srv.Close()
		_, err := DiscoverDeviceConfig(context.Background(), srv.URL, false)
		if err == nil || !strings.Contains(err.Error(), "token_endpoint") {
			t.Fatalf("err = %v, want token_endpoint rejection", err)
		}
	})
}

func TestDiscoverDeviceConfig_BadRequestURL(t *testing.T) {
	// A control character in the API URL fails at request construction, before
	// any network call.
	if _, err := DiscoverDeviceConfig(context.Background(), "http://bad\n", false); err == nil {
		t.Fatal("expected request-build error for control-char API URL")
	}
}

// --- validateEndpoint url.Parse failure ------------------------------------

func TestValidateEndpoint_UnparseableURL(t *testing.T) {
	err := validateEndpoint("token_endpoint", "https://%zz/x", "https://api", false)
	if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("err = %v, want parse failure", err)
	}
}

// --- requestDeviceCode -----------------------------------------------------

func TestRequestDeviceCode(t *testing.T) {
	t.Run("success defaults interval", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("content-type = %q", ct)
			}
			_ = json.NewEncoder(w).Encode(deviceCodeResponse{
				DeviceCode: "dc", UserCode: "WXYZ", ExpiresIn: 300, Interval: 0,
			})
		}))
		defer srv.Close()
		dc, err := requestDeviceCode(context.Background(),
			&DeviceConfig{DeviceAuthorizationEndpoint: srv.URL, ClientID: "c"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if dc.Interval != 5 {
			t.Errorf("interval = %d, want defaulted 5", dc.Interval)
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()
		_, err := requestDeviceCode(context.Background(),
			&DeviceConfig{DeviceAuthorizationEndpoint: srv.URL, ClientID: "c"}, true)
		if err == nil || !strings.Contains(err.Error(), "device authorization failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{ nope"))
		}))
		defer srv.Close()
		_, err := requestDeviceCode(context.Background(),
			&DeviceConfig{DeviceAuthorizationEndpoint: srv.URL, ClientID: "c"}, true)
		if err == nil || !strings.Contains(err.Error(), "cannot parse device code response") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := requestDeviceCode(context.Background(),
			&DeviceConfig{DeviceAuthorizationEndpoint: url, ClientID: "c"}, true)
		if err == nil || !strings.Contains(err.Error(), "cannot reach device authorization endpoint") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad endpoint fails request build", func(t *testing.T) {
		_, err := requestDeviceCode(context.Background(),
			&DeviceConfig{DeviceAuthorizationEndpoint: "http://bad\n/endpoint", ClientID: "c"}, true)
		if err == nil {
			t.Fatal("expected request-build error for control-char URL")
		}
	})
}

// --- pollForToken remaining branches ---------------------------------------

func TestPollForToken_TransportErrorThenSuccess(t *testing.T) {
	// First call: hijack and drop the connection so the client's Do() errors
	// (exercises the "network error -> keep polling" branch). Second call:
	// return a token.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter is not a Hijacker")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "ax", ExpiresIn: 3600})
	}))
	defer srv.Close()

	tok, err := pollForToken(context.Background(),
		&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
		&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
		true,
	)
	if err != nil || tok.AccessToken != "ax" {
		t.Fatalf("err=%v tok=%+v", err, tok)
	}
}

func TestPollForToken_RequestBuildError(t *testing.T) {
	_, err := pollForToken(context.Background(),
		&DeviceConfig{TokenEndpoint: "http://bad\n/token", ClientID: "c"},
		&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
		true,
	)
	if err == nil {
		t.Fatal("expected request-build error for control-char token endpoint")
	}
}

func TestPollForToken_UnexpectedErrorSurfacesDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(tokenResponse{
			Error:            "invalid_grant",
			ErrorDescription: "device code not recognized",
		})
	}))
	defer srv.Close()
	_, err := pollForToken(context.Background(),
		&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
		&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "device code not recognized") {
		t.Fatalf("err = %v, want error_description surfaced", err)
	}
}

// --- LoginDevice end-to-end ------------------------------------------------

// deviceServer builds an httptest server that plays all three roles: the
// device-config discovery endpoint, the device-authorization endpoint, and
// the token endpoint. tokenHandler decides how the token endpoint responds.
func deviceServer(t *testing.T, tokenHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/api/v0_1/auth/keycloak/device-config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceConfig{
			DeviceAuthorizationEndpoint: base + "/device",
			TokenEndpoint:               base + "/token",
			ClientID:                    "dbgorilla-cli",
			VerificationURI:             base + "/activate",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(deviceCodeResponse{
			DeviceCode:              "dc",
			UserCode:                "WXYZ",
			VerificationURIComplete: base + "/activate?user_code=WXYZ",
			ExpiresIn:               3600,
			Interval:                0,
		})
	})
	mux.HandleFunc("/token", tokenHandler)
	srv := httptest.NewServer(mux)
	base = srv.URL
	return srv
}

func TestLoginDevice_HappyPath(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()
	fastPoll(t)
	lastURL := stubBrowser(t)

	srv := deviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "ax",
			RefreshToken: "rx",
			ExpiresIn:    3600,
		})
	})
	defer srv.Close()

	tok, err := LoginDevice(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if tok.AccessToken != "ax" || tok.RefreshToken != "rx" {
		t.Errorf("tokens = %+v", tok)
	}
	// Device/SSO tokens must record the Keycloak endpoint + client for refresh.
	if tok.TokenEndpoint != srv.URL+"/token" || tok.ClientID != "dbgorilla-cli" {
		t.Errorf("refresh metadata = endpoint %q client %q", tok.TokenEndpoint, tok.ClientID)
	}
	if !strings.Contains(*lastURL, "user_code=WXYZ") {
		t.Errorf("browser opened %q, want the verification-complete URL", *lastURL)
	}
	// Persisted.
	stored, err := LoadTokens()
	if err != nil || stored.AccessToken != "ax" {
		t.Errorf("stored=%+v err=%v", stored, err)
	}
}

// When the IdP omits verification_uri_complete, the flow falls back to the
// plain verification_uri for the displayed/opened URL.
func TestLoginDevice_FallsBackToVerificationURI(t *testing.T) {
	isolatedConfigDir(t)
	keyring.MockInit()
	fastPoll(t)
	lastURL := stubBrowser(t)

	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/api/v0_1/auth/keycloak/device-config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceConfig{
			DeviceAuthorizationEndpoint: base + "/device",
			TokenEndpoint:               base + "/token",
			ClientID:                    "dbgorilla-cli",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(deviceCodeResponse{
			DeviceCode:      "dc",
			UserCode:        "WXYZ",
			VerificationURI: base + "/activate", // no VerificationURIComplete
			ExpiresIn:       3600,
			Interval:        0,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "ax", ExpiresIn: 3600})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	defer srv.Close()

	if _, err := LoginDevice(context.Background(), srv.URL, true); err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if !strings.HasSuffix(*lastURL, "/activate") {
		t.Errorf("opened URL = %q, want the plain verification_uri", *lastURL)
	}
}

func TestLoginDevice_DiscoveryFails(t *testing.T) {
	stubBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := LoginDevice(context.Background(), srv.URL, false); err == nil {
		t.Fatal("expected discovery error")
	}
}

func TestLoginDevice_DeviceCodeRequestFails(t *testing.T) {
	stubBrowser(t)
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/api/v0_1/auth/keycloak/device-config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceConfig{
			DeviceAuthorizationEndpoint: base + "/device",
			TokenEndpoint:               base + "/token",
			ClientID:                    "dbgorilla-cli",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	defer srv.Close()

	if _, err := LoginDevice(context.Background(), srv.URL, true); err == nil {
		t.Fatal("expected device-code request error")
	}
}

func TestLoginDevice_PollingDenied(t *testing.T) {
	fastPoll(t)
	stubBrowser(t)
	srv := deviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{Error: "access_denied"})
	})
	defer srv.Close()
	if _, err := LoginDevice(context.Background(), srv.URL, true); err == nil ||
		!strings.Contains(err.Error(), "denied") {
		t.Fatal("expected access_denied error")
	}
}

func TestLoginDevice_StoreFails(t *testing.T) {
	// Config dir unresolvable + broken keychain -> StoreTokens fails after a
	// successful poll, so LoginDevice surfaces "cannot store tokens".
	fileAsBase := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileAsBase, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", fileAsBase)
	t.Setenv("HOME", fileAsBase)
	keyring.MockInitWithError(errors.New("no keychain"))
	fastPoll(t)
	stubBrowser(t)

	srv := deviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "ax", ExpiresIn: 3600})
	})
	defer srv.Close()

	if _, err := LoginDevice(context.Background(), srv.URL, true); err == nil ||
		!strings.Contains(err.Error(), "cannot store tokens") {
		t.Fatalf("err = %v, want store failure", err)
	}
}

// --- httpClient CheckRedirect ----------------------------------------------

func TestHTTPClient_RefusesNonHTTPSRedirect(t *testing.T) {
	// Plain-http server that 302s to another http URL. With insecure=false the
	// redirect policy must refuse the non-https hop.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	_, err := httpClient(false).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "refusing redirect to non-https") {
		t.Fatalf("err = %v, want refusal", err)
	}
}

func TestHTTPClient_StopsAfterTenRedirects(t *testing.T) {
	// Self-redirect loop; insecure=true skips the https check so we reach the
	// 10-hop guard.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	_, err := httpClient(true).Get(srv.URL + "/loop")
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want redirect-limit error", err)
	}
}

// --- firstNonEmpty ---------------------------------------------------------

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want third", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
