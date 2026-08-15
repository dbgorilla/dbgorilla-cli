package cmd

import (
	"encoding/json"
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
