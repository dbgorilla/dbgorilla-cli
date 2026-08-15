package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Auto-detecting the login mode used to fetch and validate device-config, then
// throw the result away so the device flow fetched and validated it again.
// Every endpoint check ran twice, so the host-mismatch warning printed twice
// per endpoint -- four alarming blocks, for two conditions, on the first
// command a new user runs. Fetch once and reuse.
func TestRunLogin_AutoDetectFetchesDeviceConfigOnce(t *testing.T) {
	isolate(t)

	var configHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(deviceConfigPath, func(w http.ResponseWriter, _ *http.Request) {
		configHits.Add(1)
		// Endpoints on a different host from the API, which is what triggers
		// the warning this test exists to count.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_authorization_endpoint": "https://auth.example.test/device",
			"token_endpoint":                "https://auth.example.test/token",
			"client_id":                     "dbgorilla-cli",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "insecure", "true") // the test server is http

	// The flow fails at the device-authorization request (auth.example.test is
	// unroutable), which is fine: discovery has already happened by then.
	var err error
	_ = capture(t, func() { err = runLogin(c, nil) })
	if err == nil {
		t.Fatal("expected the device request to fail against an unroutable auth host")
	}

	if got := configHits.Load(); got != 1 {
		t.Errorf("device-config fetched %d times, want exactly 1 -- each extra fetch reprints every endpoint warning", got)
	}
}

// Forcing --mode sso skips auto-detection, so the flow must still discover the
// config itself rather than relying on a value the caller never fetched.
func TestRunLogin_ForcedSSOStillDiscovers(t *testing.T) {
	isolate(t)

	var configHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(deviceConfigPath, func(w http.ResponseWriter, _ *http.Request) {
		configHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "mode", "sso")

	var err error
	_ = capture(t, func() { err = runLogin(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "device-config") {
		t.Fatalf("want a device-config error, got %v", err)
	}
	if got := configHits.Load(); got != 1 {
		t.Errorf("device-config fetched %d times under --mode sso, want 1", got)
	}
}

// Auto-detect must tell a 404 apart from "we never reached the API".
//
// A 404 means the deployment answered and has no SSO, so password mode is
// right. A stale URL that redirects to a different host, or something that is
// not the API answering, means the deployment was never found -- and dropping
// into a password prompt there asks for credentials on behalf of a deployment
// we could not reach, while burying the one error that says how to fix it.
func TestRunLogin_AutoDetectDistinguishesNoSSOFromNoAPI(t *testing.T) {
	t.Run("404 falls back to password mode", func(t *testing.T) {
		isolate(t)
		mux := http.NewServeMux()
		mux.HandleFunc(deviceConfigPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := loginTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		setStdin(t, "") // password mode reads from stdin and gets EOF

		var err error
		out := capture(t, func() { err = runLogin(c, nil) })
		if err == nil {
			t.Fatal("expected the password prompt to fail on empty stdin")
		}
		// It got as far as prompting, which is the point: SSO is genuinely absent.
		if !strings.Contains(out, "Tenant") && !strings.Contains(out, "Account") {
			t.Errorf("a 404 should reach the password prompt, got: %s", out)
		}
	})

	t.Run("a moved deployment surfaces the redirect instead of prompting", func(t *testing.T) {
		isolate(t)
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<!doctype html><html><body>app</body></html>")
		}))
		defer elsewhere.Close()

		moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL, http.StatusFound)
		}))
		defer moved.Close()

		c := loginTestCmd()
		mustSet(t, c, "api-url", moved.URL)
		mustSet(t, c, "insecure", "true") // test servers are http
		setStdin(t, "")

		var err error
		out := capture(t, func() { err = runLogin(c, nil) })
		if err == nil {
			t.Fatal("expected the redirect to be reported")
		}
		if !strings.Contains(err.Error(), elsewhere.URL) {
			t.Errorf("should name where it was redirected, got: %v", err)
		}
		if !strings.Contains(err.Error(), "dbg config set api-url") {
			t.Errorf("should say how to point there, got: %v", err)
		}
		// The user must NOT be asked for credentials for a deployment we could
		// not find.
		if strings.Contains(out, "Tenant") || strings.Contains(out, "Account") {
			t.Errorf("must not fall through to a password prompt, got: %s", out)
		}
	})
}
