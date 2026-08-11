package api

import (
	"net/http"
	"strings"
	"testing"
)

// A proxy, SPA catch-all or login portal answering instead of the API used to
// paint the terminal with markup. Name the situation instead.
func TestHTTPError_HTMLBodyIsSummarizedNotDumped(t *testing.T) {
	page := `<!DOCTYPE html>
<html><head><title>DBGorilla</title></head>
<body><div id="root"></div><script src="/assets/index.js"></script></body>
</html>`
	err := httpError("deleting collector", http.StatusNotImplemented, []byte(page))
	msg := err.Error()

	if strings.Contains(msg, "<html") || strings.Contains(msg, "<script") {
		t.Errorf("raw HTML reached the user:\n%s", msg)
	}
	if !strings.Contains(msg, "HTML response") || !strings.Contains(msg, "config get api-url") {
		t.Errorf("should name the situation and how to check it, got:\n%s", msg)
	}
	if !strings.Contains(msg, "501") {
		t.Errorf("status code should survive, got:\n%s", msg)
	}
}

func TestHTTPError_JSONBodyPassesThroughTruncated(t *testing.T) {
	short := httpError("provisioning collector", 400, []byte(`{"detail":"tenant quota exceeded"}`)).Error()
	if !strings.Contains(short, "tenant quota exceeded") {
		t.Errorf("a real API error must survive intact, got:\n%s", short)
	}

	long := httpError("provisioning collector", 400, []byte(`{"detail":"`+strings.Repeat("x", 2000)+`"}`)).Error()
	if len(long) > 900 {
		t.Errorf("long body should be truncated, got %d chars", len(long))
	}
	if !strings.Contains(long, "truncated") {
		t.Errorf("truncation should be visible, got:\n%s", long)
	}
}

func TestHTTPError_EmptyBody(t *testing.T) {
	if msg := httpError("x", 502, nil).Error(); !strings.Contains(msg, "empty response body") {
		t.Errorf("got %q", msg)
	}
}
