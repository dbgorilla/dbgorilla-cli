package cmd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
)

type resp struct {
	code int
	body string
}

// routingServer routes by exact request path (via ServeMux).
func routingServer(t *testing.T, routes map[string]resp) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, r := range routes {
		r := r
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(r.code)
			_, _ = io.WriteString(w, r.body)
		})
	}
	return httptest.NewServer(mux)
}

const (
	authPath = "/api/v0_1/auth/user"
	mcpPath  = "/api/v0_1/client_api_keys/mcp-api-access"
)

func TestCheckAuthAndAPI(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		isolate(t)
		c := baseCmd()
		mustSet(t, c, "api-url", "http://127.0.0.1:1") // nothing listening
		ok, msg, _ := checkAuthAndAPI(c, "http://127.0.0.1:1")
		if ok || !strings.Contains(msg, "cannot reach") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("401 -> token expired", func(t *testing.T) {
		isolate(t)
		srv := statusServer(t, 401, "")
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		ok, msg, _ := checkAuthAndAPI(c, srv.URL)
		if ok || !strings.Contains(msg, "token expired") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("500 -> HTTP status", func(t *testing.T) {
		isolate(t)
		srv := statusServer(t, 500, "")
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		ok, msg, _ := checkAuthAndAPI(c, srv.URL)
		if ok || !strings.Contains(msg, "HTTP 500") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("200 -> identity", func(t *testing.T) {
		isolate(t)
		srv := statusServer(t, 200, `{"email":"a@b.com","organization":"Acme"}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		ok, msg, _ := checkAuthAndAPI(c, srv.URL)
		if !ok || !strings.Contains(msg, "a@b.com") || !strings.Contains(msg, "Acme") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	// doctor prints the role and the ids under the check line, and gets them
	// from this return value rather than by parsing the message back apart.
	t.Run("200 -> the identity itself, not just the rendered line", func(t *testing.T) {
		isolate(t)
		srv := statusServer(t, 200,
			`{"email":"a@b.com","organization":"Acme","id":"u-1","tenant_id":"t-9","role":"admin"}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		ok, _, u := checkAuthAndAPI(c, srv.URL)
		if !ok {
			t.Fatal("want ok")
		}
		if u.Role != "admin" || u.UserID != "u-1" || u.TenantID != "t-9" {
			t.Errorf("identity not returned: %+v", u)
		}
	})

	// Nothing here should read the identity on a failure path, but a zero value
	// is a safer thing to hand back than a half-populated struct from a body
	// that was never a UserInfo.
	t.Run("failure returns a zero identity", func(t *testing.T) {
		isolate(t)
		srv := statusServer(t, 500, `{"email":"a@b.com","role":"admin"}`)
		defer srv.Close()
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		ok, _, u := checkAuthAndAPI(c, srv.URL)
		if ok {
			t.Fatal("want failure")
		}
		if u != (api.UserInfo{}) {
			t.Errorf("want zero identity, got %+v", u)
		}
	})
}

// The identity block has to indent to where printCheck's detail text starts,
// or it reads as three more checks rather than as detail about the last one.
func TestIdentityDetailLinesUpWithTheCheckDetail(t *testing.T) {
	u := api.UserInfo{Role: "admin", UserID: "u-1", TenantID: "t-9"}
	var buf bytes.Buffer
	printIdentityDetail(&buf, u, strings.Repeat(" ", checkDetailColumn))

	// Measure where printCheck actually starts its detail, by running it rather
	// than by restating its format string here. A test that restated the format
	// would keep passing after someone widened the name column.
	// Colour codes take no columns on screen but do count in len(), so measure
	// with them off. Restored afterwards: this is process-global.
	defer style.SetEnabled(style.Enabled())
	style.SetEnabled(false)
	line := strings.TrimRight(capture(t, func() {
		printCheck("Auth + API", true, "MARKER")
	}), "\n")
	if got := strings.Index(line, "MARKER"); got != checkDetailColumn {
		t.Fatalf("checkDetailColumn is %d but printCheck starts detail at %d (%q)",
			checkDetailColumn, got, line)
	}

	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if got := len(l) - len(strings.TrimLeft(l, " ")); got != checkDetailColumn {
			t.Errorf("line %q indents %d, want %d", l, got, checkDetailColumn)
		}
	}
}

func TestCheckMCPKey(t *testing.T) {
	isolate(t)
	cases := []struct {
		name    string
		code    int
		body    string
		wantOK  bool
		wantMsg string
	}{
		{"exists", 200, `"a-key"`, true, "exists"},
		{"empty", 200, `""`, false, "no key minted"},
		{"500", 500, "", false, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := statusServer(t, tc.code, tc.body)
			defer srv.Close()
			c := baseCmd()
			mustSet(t, c, "api-url", srv.URL)
			ok, msg := checkMCPKey(c)
			if ok != tc.wantOK || !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("ok=%v msg=%q", ok, msg)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		c := baseCmd()
		mustSet(t, c, "api-url", "http://127.0.0.1:1")
		ok, msg := checkMCPKey(c)
		if ok || !strings.Contains(msg, "cannot check") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})
}

func writeJSONFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckClientConfigured_FileBased(t *testing.T) {
	entry := `{"mcpServers":{"dbgorilla":{"url":"x"}}}`
	cases := []struct {
		name    string
		w       fakeWriter
		wantOK  bool
		wantMsg string
	}{
		{"config path error", fakeWriter{slug: "cursor", pathErr: os.ErrPermission}, false, "cannot resolve config path"},
		{"missing file", fakeWriter{slug: "cursor", path: filepath.Join(t.TempDir(), "none.json")}, false, "no config at"},
		{"invalid json", fakeWriter{slug: "cursor", path: writeJSONFile(t, "not json")}, false, "not valid JSON"},
		{"no top-level block", fakeWriter{slug: "cursor", path: writeJSONFile(t, `{"other":{}}`)}, false, "no mcpServers block"},
		{"no dbgorilla entry", fakeWriter{slug: "cursor", path: writeJSONFile(t, `{"mcpServers":{"foo":{}}}`)}, false, "no 'dbgorilla' entry"},
		{"present", fakeWriter{slug: "cursor", path: writeJSONFile(t, entry)}, true, "entry present"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := checkClientConfigured(tc.w)
			if ok != tc.wantOK || !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("ok=%v msg=%q", ok, msg)
			}
		})
	}
}

func TestCheckClientConfigured_ClaudeCode(t *testing.T) {
	t.Run("registered via claude mcp list", func(t *testing.T) {
		stubLookPath(t, true)
		stubExec(t, "dbgorilla  https://dep/mcp/", 0)
		ok, msg := checkClientConfigured(fakeWriter{slug: "claude-code"})
		if !ok || !strings.Contains(msg, "registered") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("not registered", func(t *testing.T) {
		stubLookPath(t, true)
		stubExec(t, "some-other-server", 0)
		ok, msg := checkClientConfigured(fakeWriter{slug: "claude-code"})
		if ok || !strings.Contains(msg, "not registered") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("no claude CLI falls back to config file", func(t *testing.T) {
		stubLookPath(t, false)
		p := writeJSONFile(t, `{"mcpServers":{"dbgorilla":{"url":"x"}}}`)
		ok, msg := checkClientConfigured(fakeWriter{slug: "claude-code", path: p})
		if !ok || !strings.Contains(msg, "entry present") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})
}

func TestPrintCheck(t *testing.T) {
	okOut := capture(t, func() { printCheck("Thing", true, "all good") })
	if !strings.Contains(okOut, "[ OK ]") || !strings.Contains(okOut, "Thing") {
		t.Errorf("ok render wrong: %q", okOut)
	}
	failOut := capture(t, func() { printCheck("Thing", false, "broken") })
	if !strings.Contains(failOut, "[FAIL]") {
		t.Errorf("fail render wrong: %q", failOut)
	}
}

func TestRunDoctor(t *testing.T) {
	t.Run("no api url falls back to production default", func(t *testing.T) {
		isolate(t)
		stubDetect(t) // no clients
		var err error
		out := capture(t, func() { err = runDoctor(baseCmd(), nil) })
		if err != errDoctorFailed {
			t.Fatalf("err=%v, want errDoctorFailed (not signed in)", err)
		}
		if !strings.Contains(out, config.DefaultAPIURL) || !strings.Contains(out, "source: default") {
			t.Errorf("out=%q, want the default URL reported with source: default", out)
		}
	})

	t.Run("not signed in fails", func(t *testing.T) {
		isolate(t)
		stubDetect(t) // no clients
		c := baseCmd()
		mustSet(t, c, "api-url", "https://dep.example")
		var err error
		out := capture(t, func() { err = runDoctor(c, nil) })
		if err != errDoctorFailed {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(out, "not signed in") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("all green", func(t *testing.T) {
		home := isolate(t)
		writeTokens(t)
		// Simulate the keychain-unavailable fallback file so the informational
		// token-storage check also fires.
		dir, _ := config.Dir()
		_ = dir // ensured to exist
		if err := os.WriteFile(filepath.Join(home, "dbgorilla", "credentials.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		srv := routingServer(t, map[string]resp{
			authPath: {200, `{"email":"dev@acme.com","organization":"Acme"}`},
			mcpPath:  {200, `"a-key"`},
		})
		defer srv.Close()
		cfgFile := writeJSONFile(t, `{"mcpServers":{"dbgorilla":{"url":"x"}}}`)
		stubDetect(t, fakeWriter{name: "Cursor", slug: "cursor", path: cfgFile})
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		var err error
		out := capture(t, func() { err = runDoctor(c, nil) })
		if err != nil {
			t.Fatalf("want green, err=%v\n%s", err, out)
		}
		if !strings.Contains(out, "All checks passed") {
			t.Errorf("out=%q", out)
		}
		if !strings.Contains(out, "Token storage") {
			t.Errorf("fallback-file info line missing:\n%s", out)
		}
	})

	t.Run("auth + key failures reported", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := routingServer(t, map[string]resp{
			authPath: {401, ""},
			mcpPath:  {401, ""},
		})
		defer srv.Close()
		stubDetect(t) // no clients
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		var err error
		out := capture(t, func() { err = runDoctor(c, nil) })
		if err != errDoctorFailed {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(out, "token expired") || !strings.Contains(out, "no supported clients detected") {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("hint-only client is informational, not a failure", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		srv := routingServer(t, map[string]resp{
			authPath: {200, `{"email":"dev@acme.com","organization":"Acme"}`},
			mcpPath:  {200, `"a-key"`},
		})
		defer srv.Close()
		stubDetect(t, fakeHinter{name: "Claude Desktop", slug: "claude-desktop"})
		c := baseCmd()
		mustSet(t, c, "api-url", srv.URL)
		var err error
		out := capture(t, func() { err = runDoctor(c, nil) })
		if err != nil {
			t.Fatalf("hint-only client must not fail doctor, err=%v\n%s", err, out)
		}
		if !strings.Contains(out, "manual setup") {
			t.Errorf("out=%q", out)
		}
	})
}
