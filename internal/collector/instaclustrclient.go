package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/httpx"
)

// The NetApp Instaclustr Cluster Management API. Plain REST with HTTP basic
// auth: the console username plus a provisioning API key. The API is
// dynamically rate limited (~1 request/second baseline), and everything this
// CLI does is a handful of calls, so there is no retry machinery — the error
// says what to do and the user re-runs.
const (
	instaclustrAPIBase     = "https://api.instaclustr.com"
	instaclustrHTTPTimeout = 20 * time.Second
)

// InstaclustrCreds is the basic-auth pair for the Cluster Management API.
type InstaclustrCreds struct {
	Username string
	APIKey   string
}

// instaclustrClient is the package-level HTTP client, a test seam like
// registryClient. RedirectPolicy refuses https→http downgrades.
var instaclustrClient = &http.Client{
	Timeout:       instaclustrHTTPTimeout,
	CheckRedirect: httpx.RedirectPolicy(false),
}

// errICNotFound marks a 404 from the Instaclustr API — usually a wrong
// cluster id or a key from a different Instaclustr account.
var errICNotFound = errors.New("not found")

// icStatusError carries the HTTP status of a non-2xx API response so callers
// can branch on status (409 dedup) without string-matching message text.
type icStatusError struct {
	status int
	err    error
}

func (e *icStatusError) Error() string { return e.err.Error() }
func (e *icStatusError) Unwrap() error { return e.err }

// icStatus returns the HTTP status inside err, or 0 when err carries none.
func icStatus(err error) int {
	var se *icStatusError
	if errors.As(err, &se) {
		return se.status
	}
	return 0
}

// icSend performs one authenticated request and returns the response body for
// 2xx, or an error carrying the API's own message for anything else.
func icSend(ctx context.Context, creds InstaclustrCreds, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, instaclustrAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(creds.Username, creds.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := instaclustrClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("instaclustr API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("instaclustr API response read failed: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}
	return data, &icStatusError{status: resp.StatusCode, err: icAPIError(resp.StatusCode, data)}
}

// icGetJSON is icSend(GET) plus a JSON decode into out.
func icGetJSON(ctx context.Context, creds InstaclustrCreds, path string, out any) error {
	data, err := icSend(ctx, creds, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("instaclustr API returned unexpected JSON: %w", err)
	}
	return nil
}

// icAPIError maps a non-2xx status to an error that tells the user what to do.
// The response body is included when it looks like the API's own message.
func icAPIError(status int, body []byte) error {
	detail := icErrorDetail(body)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("instaclustr API rejected the credentials (HTTP %d)%s. "+
			"Check the username and API key — this call needs a provisioning API key "+
			"(Instaclustr console: gear icon, then Account Settings, then API Keys)", status, detail)
	case http.StatusNotFound:
		return fmt.Errorf("%w (HTTP 404)%s. Check the cluster id and that the API key "+
			"belongs to the account that owns the cluster", errICNotFound, detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("instaclustr API rate limit hit (HTTP 429)%s. Wait a few "+
			"seconds and re-run", detail)
	default:
		return fmt.Errorf("instaclustr API error (HTTP %d)%s", status, detail)
	}
}

// icErrorDetail extracts the API's message text from an error body. Their
// errors come as {"errors":[{"message":...}]} or {"message":...}; anything
// else is shown raw (truncated) so the user is never left with a bare status.
func icErrorDetail(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	var envelope struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
			return ": " + envelope.Errors[0].Message
		}
		if envelope.Message != "" {
			return ": " + envelope.Message
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return ": " + s
}
