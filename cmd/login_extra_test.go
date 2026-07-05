package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Auto-detect chooses SSO when the device-config endpoint is present. We stop
// the flow at the device-authorization step (400) to keep the test fast while
// still exercising the auto-detect -> sso branch.
func TestRunLogin_AutoDetectChoosesSSO(t *testing.T) {
	isolate(t)
	mux := http.NewServeMux()
	mux.HandleFunc(deviceConfigPath, func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"device_authorization_endpoint":"`+base+`/device",`+
			`"token_endpoint":"`+base+`/token","client_id":"c"}`)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400) // device authorization refused -> flow ends quickly
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "insecure", "true") // allow the http device endpoints
	var err error
	out := capture(t, func() { err = runLogin(c, nil) })
	if err == nil {
		t.Fatal("expected the SSO device flow to fail at authorization")
	}
	if !strings.Contains(out, "Signing in via SSO") {
		t.Errorf("auto-detect should have selected SSO mode:\n%s", out)
	}
}

// A 200 /auth/user body that is not valid UserInfo JSON must not fail login.
func TestRunLogin_SignedInIdentityUnparseable(t *testing.T) {
	isolate(t)
	srv := routingServer(t, map[string]resp{
		tokenPath: {200, `{"access_token":"tok","expires_in":3600}`},
		authPath:  {200, `"a bare json string, not a user object"`},
	})
	defer srv.Close()
	setStdin(t, "pw\n")
	c := loginTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("mode", "password")
	_ = c.Flags().Set("tenant", "acme")
	_ = c.Flags().Set("account", "dev")
	var err error
	out := capture(t, func() { err = runLogin(c, nil) })
	if err != nil {
		t.Fatalf("login should succeed despite unparseable identity, err=%v", err)
	}
	if !strings.Contains(out, "Signed in.") {
		t.Errorf("out=%q", out)
	}
}
