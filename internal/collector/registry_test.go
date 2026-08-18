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
