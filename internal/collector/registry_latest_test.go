package collector

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewestRelease_OrdersAsVersionsNotStrings(t *testing.T) {
	// As strings "0.9.0" > "0.12.1"; as versions it is the other way round.
	got, ok := NewestRelease([]string{"0.9.0", "0.12.1", "0.10.1", "0.1.17"})
	if !ok || got != "0.12.1" {
		t.Errorf("got %q (ok=%v), want 0.12.1", got, ok)
	}
}

func TestNewestRelease_IgnoresPreReleasesAndMovingTags(t *testing.T) {
	tags := []string{
		"0.12.1",
		"0.13.0-rc.1",         // pre-release: not for customers
		"0.0.0-pipeline-test", // pre-release
		"1.0.0+build.5",       // build metadata: not a plain release tag
		"latest",              // moving tag
		"edge",                // moving tag
		"3f9c2ab",             // commit sha
		"1.2",                 // not X.Y.Z
		"v",                   // junk
	}
	got, ok := NewestRelease(tags)
	if !ok || got != "0.12.1" {
		t.Errorf("got %q (ok=%v), want 0.12.1", got, ok)
	}
}

func TestNewestRelease_KeepsALeadingV(t *testing.T) {
	// The tag is returned as published, so the image reference resolves.
	got, ok := NewestRelease([]string{"v0.2.0", "0.1.0"})
	if !ok || got != "v0.2.0" {
		t.Errorf("got %q (ok=%v), want v0.2.0", got, ok)
	}
}

func TestNewestRelease_NoReleaseAtAll(t *testing.T) {
	if got, ok := NewestRelease([]string{"latest", "1.0.0-rc.1"}); ok {
		t.Errorf("want none, got %q", got)
	}
	if _, ok := NewestRelease(nil); ok {
		t.Error("an empty list has no release")
	}
}

// A fake registry that wants a token, then serves the tag list over two pages
// the way a real registry paginates (a relative Link header).
func fakeTagRegistry(t *testing.T, pages [][]string) string {
	t.Helper()
	var host string
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	})
	mux.HandleFunc("/v2/dbg-collector/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+host+`/oauth2/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page := 0
		if r.URL.Query().Get("last") != "" {
			page = 1
		}
		if page+1 < len(pages) {
			w.Header().Set("Link", `</v2/dbg-collector/tags/list?last=x&n=2>; rel="next"`)
		}
		body := `{"name":"dbg-collector","tags":[`
		for i, tag := range pages[page] {
			if i > 0 {
				body += ","
			}
			body += `"` + tag + `"`
		}
		_, _ = w.Write([]byte(body + `]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host = srv.URL
	return srv.URL
}

func TestLatestRelease_ReadsEveryPage(t *testing.T) {
	// The newest release is on the second page; stopping at the first would
	// install 0.9.0.
	pointClientAt(t, fakeTagRegistry(t, [][]string{
		{"0.1.0", "0.9.0", "latest"},
		{"0.12.1", "0.13.0-rc.1"},
	}))
	image, version, err := LatestRelease()
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.12.1" || image != ImageRepo+":0.12.1" {
		t.Errorf("got %q / %q, want %s:0.12.1", image, version, ImageRepo)
	}
}

func TestLatestRelease_NoReleaseIsAnError(t *testing.T) {
	pointClientAt(t, fakeTagRegistry(t, [][]string{{"latest", "0.0.0-pipeline-test"}}))
	if image, _, err := LatestRelease(); err == nil {
		t.Errorf("want an error, got %q", image)
	}
}

func TestLatestRelease_RegistryErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	pointClientAt(t, srv.URL)
	if image, _, err := LatestRelease(); err == nil {
		t.Errorf("want an error, got %q", image)
	}
}

func TestLatestRelease_UnreachableRegistryIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening any more
	pointClientAt(t, url)
	if image, _, err := LatestRelease(); err == nil {
		t.Errorf("want an error, got %q", image)
	}
}

func TestNextLink(t *testing.T) {
	cases := map[string]string{
		`</v2/r/tags/list?last=0.1.0&n=2&orderby=>; rel="next"`: "/v2/r/tags/list?last=0.1.0&n=2&orderby=",
		``:                                   "",
		`</a>; rel="prev"`:                   "",
		`</a>; rel="prev", </b>; rel="next"`: "/b",
	}
	for in, want := range cases {
		if got := nextLink(in); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", in, got, want)
		}
	}
}
