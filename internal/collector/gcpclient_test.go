package collector

import (
	"context"
	"net/http"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// deadlineProbe records whether each request carried a context deadline.
type deadlineProbe struct {
	deadlines []bool
}

func (p *deadlineProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	_, ok := req.Context().Deadline()
	p.deadlines = append(p.deadlines, ok)
	return gcpResponse(200, `{}`), nil
}

// Every API exchange is bounded, so a blackholed connection cannot hang a
// command forever; a caller's own deadline is kept.
func TestGcpDo_BoundsEveryRequest(t *testing.T) {
	probe := &deadlineProbe{}
	cfg := gcpConfig{http: &http.Client{Transport: probe}, tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"})}

	if err := gcpDo(context.Background(), cfg, http.MethodGet, "https://x/", nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := gcpDo(ctx, cfg, http.MethodGet, "https://x/", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := gcpTokenEmail(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for i, ok := range probe.deadlines {
		if !ok {
			t.Errorf("request %d carried no deadline", i)
		}
	}
}
