package collector

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A fake registry: challenges for a token, then answers the manifest HEAD with
// a digest. Enough of the OCI distribution flow to exercise the real path.
func fakeRegistry(t *testing.T, digest string, requireToken bool) (host string, tokenCalls *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		calls++
		// The scope must name the repository, or a real registry issues a token
		// that cannot read it.
		if !strings.Contains(r.URL.Query().Get("scope"), "repository:") {
			t.Errorf("token request had no repository scope: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if requireToken && r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+host+`/oauth2/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// A tag can point at a multi-platform index; asking for only the
		// single-platform types would yield a different digest.
		if !strings.Contains(r.Header.Get("Accept"), "image.index") {
			t.Errorf("Accept did not offer an index type: %s", r.Header.Get("Accept"))
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host = srv.URL
	return srv.URL, &calls
}

// pointClientAt makes https://<host>/... reach the test server instead.
func pointClientAt(t *testing.T, srvURL string) {
	t.Helper()
	orig := registryClient
	registryClient = &http.Client{
		Transport: rewriteTransport{target: strings.TrimPrefix(srvURL, "http://")},
	}
	t.Cleanup(func() { registryClient = orig })
}

type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(req)
}

const testDigest = "sha256:783f3f13899f1bb87952bffa07b8bea78c249b6365d72f639a28446f5817c261"

func TestRemoteDigest_ResolvesATag(t *testing.T) {
	srv, tokenCalls := fakeRegistry(t, testDigest, true)
	pointClientAt(t, srv)

	got, err := RemoteDigest("reg.example.com/dbg-collector:latest")
	if err != nil {
		t.Fatalf("RemoteDigest: %v", err)
	}
	want := "reg.example.com/dbg-collector:latest@" + testDigest
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if *tokenCalls != 1 {
		t.Errorf("token fetched %d times, want 1", *tokenCalls)
	}
}

// A registry that does not challenge should not be handed a token request.
func TestRemoteDigest_NoTokenNeeded(t *testing.T) {
	srv, tokenCalls := fakeRegistry(t, testDigest, false)
	pointClientAt(t, srv)

	if _, err := RemoteDigest("reg.example.com/dbg-collector:latest"); err != nil {
		t.Fatalf("RemoteDigest: %v", err)
	}
	if *tokenCalls != 0 {
		t.Errorf("token fetched %d times without a challenge, want 0", *tokenCalls)
	}
}

// An already-pinned reference is immutable. Re-resolving could only introduce
// disagreement between what was asked for and what is recorded.
func TestRemoteDigest_AlreadyPinnedIsUntouched(t *testing.T) {
	ref := "reg.example.com/dbg-collector:0.5.0@" + testDigest
	got, err := RemoteDigest(ref)
	if err != nil || got != ref {
		t.Fatalf("got (%q,%v), want the input unchanged", got, err)
	}
}

func TestRemoteDigest_Rejections(t *testing.T) {
	t.Run("no tag to resolve", func(t *testing.T) {
		if _, err := RemoteDigest("reg.example.com/dbg-collector"); err == nil {
			t.Fatal("a reference with no tag cannot be resolved")
		}
	})

	// Guessing at Docker Hub's API for an image this CLI never publishes there
	// would be a confidently wrong answer.
	t.Run("no registry host", func(t *testing.T) {
		if _, err := RemoteDigest("dbg-collector:latest"); err == nil {
			t.Fatal("a reference without a registry host must be refused")
		}
	})

	t.Run("registry error surfaces", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		pointClientAt(t, srv.URL)
		_, err := RemoteDigest("reg.example.com/dbg-collector:nope")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v, want the status surfaced", err)
		}
	})

	t.Run("a response with no digest is an error, not an empty pin", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK) // no Docker-Content-Digest
		}))
		defer srv.Close()
		pointClientAt(t, srv.URL)
		if _, err := RemoteDigest("reg.example.com/dbg-collector:latest"); err == nil {
			t.Fatal("a missing digest must not produce a reference ending in @")
		}
	})
}

func TestParseAuthChallenge(t *testing.T) {
	realm, service, ok := parseAuthChallenge(`Bearer realm="https://reg.example.com/oauth2/token",service="reg.example.com"`)
	if !ok || realm != "https://reg.example.com/oauth2/token" || service != "reg.example.com" {
		t.Errorf("got (%q,%q,%v)", realm, service, ok)
	}
	if _, _, ok := parseAuthChallenge("Basic realm=\"x\""); ok {
		t.Error("a non-Bearer challenge is not one we can answer")
	}
	if _, _, ok := parseAuthChallenge(""); ok {
		t.Error("an empty header is not a challenge")
	}
}

// --- token endpoint failure modes -----------------------------------------
//
// These are what a customer behind a proxy, or a registry mid-incident, hits.
// Each must produce an error the caller can act on rather than an empty
// reference that later reads as "no version".

func challengingRegistry(t *testing.T, tokenHandler http.HandlerFunc) string {
	t.Helper()
	var host string
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", tokenHandler)
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+host+`/oauth2/token",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host = srv.URL
	return srv.URL
}

func TestRemoteDigest_TokenEndpointFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			"token endpoint refuses",
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			"403",
		},
		{
			"token response is not JSON",
			func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>nope")) },
			"token response",
		},
		{
			"token response carries no token",
			func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
			"no token",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := challengingRegistry(t, c.handler)
			pointClientAt(t, srv)
			_, err := RemoteDigest("reg.example.com/dbg-collector:latest")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
			// The reference must never come back half-formed.
			if strings.HasSuffix(err.Error(), "@") {
				t.Error("error must not describe a truncated reference")
			}
		})
	}
}

// The older token field name is what some registries return; both must work.
func TestRemoteDigest_AcceptsEitherTokenFieldName(t *testing.T) {
	srv := challengingRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"tok"}`))
	})
	pointClientAt(t, srv)
	// The registry above always challenges, so this proves the token was sent
	// and accepted only if the second HEAD succeeds -- it will not here, but
	// the failure must be about the manifest, not about the token.
	_, err := RemoteDigest("reg.example.com/dbg-collector:latest")
	if err != nil && strings.Contains(err.Error(), "no token") {
		t.Errorf("the `token` field should be accepted, got %v", err)
	}
}

// A registry that is simply unreachable must say so, not look like a missing
// image.
func TestRemoteDigest_UnreachableRegistry(t *testing.T) {
	orig := registryClient
	registryClient = &http.Client{Transport: rewriteTransport{target: "127.0.0.1:1"}}
	t.Cleanup(func() { registryClient = orig })

	_, err := RemoteDigest("reg.example.com/dbg-collector:latest")
	if err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("err = %v, want an unreachable-registry error", err)
	}
}

// A 401 with no usable challenge is a dead end, not a token problem.
func TestRemoteDigest_UnauthorizedWithoutAChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no Www-Authenticate
	}))
	defer srv.Close()
	pointClientAt(t, srv.URL)

	_, err := RemoteDigest("reg.example.com/dbg-collector:latest")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the status reported", err)
	}
}

func TestAuthChallengeError_Message(t *testing.T) {
	e := &authChallengeError{realm: "https://reg.example.com/oauth2/token"}
	if !strings.Contains(e.Error(), "reg.example.com") {
		t.Errorf("message should name where a token comes from, got %q", e.Error())
	}
}

// A malformed challenge should not crash the parse or invent a realm.
func TestParseAuthChallenge_Malformed(t *testing.T) {
	if _, _, ok := parseAuthChallenge("Bearer service=\"reg\""); ok {
		t.Error("a challenge with no realm cannot be answered")
	}
	if _, _, ok := parseAuthChallenge("Bearer garbage-without-equals"); ok {
		t.Error("an unparseable challenge is not answerable")
	}
}

// --- the last error paths -------------------------------------------------
//
// Each is a "this should be impossible" branch. They are covered because an
// impossible branch that returns a zero value instead of an error is how a
// reference ends up recorded as an empty string.

// A tag carrying a character that cannot appear in a URL must be refused
// before a request is attempted.
func TestRemoteDigest_UnbuildableManifestURL(t *testing.T) {
	_, err := RemoteDigest("reg.example.com/dbg-collector:bad\ntag")
	if err == nil {
		t.Fatal("a reference that cannot form a URL must error")
	}
}

// The realm comes from the registry, so it can be anything -- including
// something that is not a usable URL.
func TestRemoteDigest_UnusableTokenRealm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Www-Authenticate", "Bearer realm=\"http://bad realm\",service=\"reg\"")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	pointClientAt(t, srv.URL)

	if _, err := RemoteDigest("reg.example.com/dbg-collector:latest"); err == nil {
		t.Fatal("an unusable realm must error rather than silently skip the token")
	}
}

// The manifest endpoint answers but the token endpoint does not.
func TestRemoteDigest_TokenEndpointUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="http://127.0.0.1:1/oauth2/token",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	orig := registryClient
	registryClient = &http.Client{Transport: tokenFailingTransport{manifest: strings.TrimPrefix(srv.URL, "http://")}}
	t.Cleanup(func() { registryClient = orig })

	_, err := RemoteDigest("reg.example.com/dbg-collector:latest")
	if err == nil || !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("err = %v, want the token endpoint named", err)
	}
}

// Routes manifest requests to the test server and leaves token requests to
// fail against the unroutable realm the challenge advertises.
type tokenFailingTransport struct{ manifest string }

func (t tokenFailingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/oauth2/token") {
		return http.DefaultTransport.RoundTrip(req)
	}
	req.URL.Scheme = "http"
	req.URL.Host = t.manifest
	return http.DefaultTransport.RoundTrip(req)
}
