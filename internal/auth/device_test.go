package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDiscoverDeviceConfig_RequiresHTTPS validates the H1 fix: discovered
// endpoint URLs must be https unless --insecure.
func TestDiscoverDeviceConfig_RequiresHTTPS(t *testing.T) {
	cases := []struct {
		name      string
		body      DeviceConfig
		insecure  bool
		wantError string
	}{
		{
			"http endpoint rejected when secure",
			DeviceConfig{
				DeviceAuthorizationEndpoint: "http://malicious.example/device",
				TokenEndpoint:               "https://idp/token",
				ClientID:                    "x",
			},
			false,
			"non-https scheme",
		},
		{
			"http endpoint accepted when insecure",
			DeviceConfig{
				DeviceAuthorizationEndpoint: "http://localhost/device",
				TokenEndpoint:               "http://localhost/token",
				ClientID:                    "x",
			},
			true,
			"",
		},
		{
			"missing required field",
			DeviceConfig{
				DeviceAuthorizationEndpoint: "https://idp/device",
				// TokenEndpoint missing
				ClientID: "x",
			},
			false,
			"missing required fields",
		},
		{
			"both endpoints https -- success",
			DeviceConfig{
				DeviceAuthorizationEndpoint: "https://idp.example/device",
				TokenEndpoint:               "https://idp.example/token",
				ClientID:                    "x",
				VerificationURI:             "https://idp.example/activate",
			},
			false,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			_, err := DiscoverDeviceConfig(context.Background(), srv.URL, tc.insecure)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantError)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantError)
				}
			}
		})
	}
}

// TestPollForToken_StateMachine exercises every documented branch of the
// device-flow token endpoint: authorization_pending -> slow_down -> success,
// access_denied, expired_token. Uses an httptest server with a call-counter
// to script the response sequence.
func TestPollForToken_StateMachine(t *testing.T) {
	t.Run("pending then success", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(tokenResponse{Error: "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(tokenResponse{
				AccessToken:  "ax",
				RefreshToken: "rx",
				ExpiresIn:    3600,
			})
		}))
		defer srv.Close()

		tok, err := pollForToken(context.Background(),
			&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
			&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
			true,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok.AccessToken != "ax" || tok.RefreshToken != "rx" {
			t.Errorf("token = %+v", tok)
		}
	})

	t.Run("slow_down bumps interval", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n < 2 {
				_ = json.NewEncoder(w).Encode(tokenResponse{Error: "slow_down"})
				return
			}
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
	})

	t.Run("access_denied terminates", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(tokenResponse{Error: "access_denied"})
		}))
		defer srv.Close()
		_, err := pollForToken(context.Background(),
			&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
			&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
			true,
		)
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("err = %v, want denial", err)
		}
	})

	t.Run("expired_token terminates", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(tokenResponse{Error: "expired_token"})
		}))
		defer srv.Close()
		_, err := pollForToken(context.Background(),
			&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
			&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
			true,
		)
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Errorf("err = %v, want expiry", err)
		}
	})

	t.Run("deadline_exceeded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(tokenResponse{Error: "authorization_pending"})
		}))
		defer srv.Close()
		// ExpiresIn 0 trips the deadline immediately.
		_, err := pollForToken(context.Background(),
			&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
			&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 0, Interval: 0},
			true,
		)
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Errorf("err = %v, want deadline expiry", err)
		}
	})

	t.Run("context cancellation respected", func(t *testing.T) {
		// Server is slow-walking. We cancel after a beat.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(tokenResponse{Error: "authorization_pending"})
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		_, err := pollForToken(ctx,
			&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "c"},
			&deviceCodeResponse{DeviceCode: "d", ExpiresIn: 30, Interval: 0},
			true,
		)
		// Either ctx.Err or the per-request canceled error is acceptable.
		if err == nil {
			t.Error("expected error on canceled ctx, got nil")
		}
	})
}

// TestValidateEndpoint_HostMismatchWarns ensures host mismatch is warned
// but not errored (Keycloak is often on a separate subdomain).
func TestValidateEndpoint_HostMismatchWarns(t *testing.T) {
	apiURL := "https://api.example.com"
	other := "https://idp.different-host.com/device"
	if err := validateEndpoint("device_authorization_endpoint", other, apiURL, false); err != nil {
		t.Errorf("host mismatch should warn not error, got: %v", err)
	}
}

func TestValidateEndpoint_InvalidURLs(t *testing.T) {
	cases := []struct {
		name, endpoint, want string
	}{
		{"missing host", "https:///device", "no host"},
		{"http (with secure)", "http://idp/device", "non-https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEndpoint("device_authorization_endpoint", tc.endpoint, "https://api", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v want %q", err, tc.want)
			}
		})
	}
}

// helper for tests that need to parse a form-encoded body
func parseForm(body string) url.Values {
	v, _ := url.ParseQuery(body)
	return v
}

// Verify httptest sees the right grant_type so we don't accidentally break
// the wire protocol.
func TestPollForToken_SendsCorrectGrantType(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"access_token":"ax","expires_in":3600}`)
	}))
	defer srv.Close()
	_, _ = pollForToken(context.Background(),
		&DeviceConfig{TokenEndpoint: srv.URL, ClientID: "myclient"},
		&deviceCodeResponse{DeviceCode: "thecode", ExpiresIn: 30, Interval: 0},
		true,
	)
	v := parseForm(got)
	if v.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant_type = %q", v.Get("grant_type"))
	}
	if v.Get("device_code") != "thecode" || v.Get("client_id") != "myclient" {
		t.Errorf("form body = %q", got)
	}
}
