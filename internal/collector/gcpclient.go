package collector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GCP access uses Application Default Credentials (golang.org/x/oauth2/google),
// so no gcloud binary is required and the operator's own credentials are reused.

// gcpConfig is what every GCP call in this package builds from.
type gcpConfig struct {
	http    *http.Client
	tokens  oauth2.TokenSource
	project string
}

var (
	gcpCfgOnce sync.Once
	gcpCfg     gcpConfig
	gcpCfgErr  error
)

// gcpRequestTimeout bounds a single API exchange that carries no deadline of
// its own.
const gcpRequestTimeout = 60 * time.Second

// loadGCPConfig resolves ADC. A variable so tests can substitute a fake
// transport.
var loadGCPConfig = loadGCPConfigDefault

func loadGCPConfigDefault(ctx context.Context) (gcpConfig, error) {
	gcpCfgOnce.Do(func() {
		creds, err := google.FindDefaultCredentials(ctx,
			"https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			gcpCfgErr = err
			return
		}
		gcpCfg = gcpConfig{
			http:    oauth2.NewClient(ctx, creds.TokenSource),
			tokens:  creds.TokenSource,
			project: creds.ProjectID,
		}
	})
	return gcpCfg, gcpCfgErr
}

// gcpCredsErr wraps a credential-resolution failure with the remediation.
func gcpCredsErr(err error) error {
	return fmt.Errorf("could not load Google Cloud credentials "+
		"(run 'gcloud auth application-default login', or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
}

// GcpAvailable returns nil when Google Cloud credentials resolve and mint a
// working token.
func GcpAvailable() error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	if _, err := gcpTokenEmail(ctx, cfg); err != nil {
		return fmt.Errorf("Google Cloud credentials aren't working "+
			"(run 'gcloud auth application-default login' to refresh them): %w", err)
	}
	return nil
}

// GcpIdentity returns the ADC principal's email when the token carries one.
func GcpIdentity() (string, error) {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return "", gcpCredsErr(err)
	}
	email, err := gcpTokenEmail(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("could not confirm the Google Cloud identity: %w", err)
	}
	if email == "" {
		return "authenticated (the credential reports no email)", nil
	}
	return email, nil
}

// GcpProject returns the project to act on when --project is not given: the
// credentials' own project (service-account keys, instance metadata), else
// GOOGLE_CLOUD_PROJECT / CLOUDSDK_CORE_PROJECT, else gcloud's active
// configuration.
func GcpProject() (string, error) {
	cfg, err := loadGCPConfig(context.Background())
	if err != nil {
		return "", gcpCredsErr(err)
	}
	if cfg.project != "" {
		return cfg.project, nil
	}
	for _, env := range []string{"GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT", "GCLOUD_PROJECT"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v, nil
		}
	}
	if p := gcloudConfigProject(); p != "" {
		return p, nil
	}
	return "", errors.New("no Google Cloud project is configured — " +
		"pass --project, or set one with 'gcloud config set project'")
}

// gcloudConfigDir is where gcloud keeps its configurations; a variable so
// tests can point it at a fixture.
var gcloudConfigDir = func() string {
	if d := os.Getenv("CLOUDSDK_CONFIG"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "gcloud")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud")
}

// gcloudConfigProject reads core/project from gcloud's active configuration.
func gcloudConfigProject() string {
	dir := gcloudConfigDir()
	if dir == "" {
		return ""
	}
	name := "default"
	if b, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil && strings.TrimSpace(string(b)) != "" {
		name = strings.TrimSpace(string(b))
	}
	f, err := os.Open(filepath.Join(dir, "configurations", "config_"+name))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	inCore := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "["):
			inCore = line == "[core]"
		case inCore:
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "project" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// gcpTokenEmail validates the token against Google's tokeninfo endpoint and
// returns the principal email when present. The token travels in the POST
// body, never in a URL.
func gcpTokenEmail(ctx context.Context, cfg gcpConfig) (string, error) {
	tok, err := cfg.tokens.Token()
	if err != nil {
		return "", err
	}
	ctx, cancel := gcpRequestContext(ctx)
	defer cancel()
	form := url.Values{"access_token": {tok.AccessToken}}
	resp, err := gcpSend(ctx, cfg, http.MethodPost, "https://oauth2.googleapis.com/tokeninfo",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// errGcpNotFound tags a 404 so callers can errors.Is it.
var errGcpNotFound = errors.New("not found")

// gcpRequestContext adds the per-request deadline unless ctx already has one.
func gcpRequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, gcpRequestTimeout)
}

// gcpSend issues one authenticated request and returns the 2xx response; the
// caller closes the body. Anything else becomes an error carrying Google's
// own message.
func gcpSend(ctx context.Context, cfg gcpConfig, method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := cfg.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, gcpAPIError(rawURL, resp)
	}
	return resp, nil
}

// gcpDo is gcpSend for the JSON APIs: body (nil for none) is marshalled and the
// response decoded into out (nil to discard it).
func gcpDo(ctx context.Context, cfg gcpConfig, method, rawURL string, body, out any) error {
	ctx, cancel := gcpRequestContext(ctx)
	defer cancel()
	var payload io.Reader
	contentType := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	resp, err := gcpSend(ctx, cfg, method, rawURL, contentType, payload)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// gcpPage is embedded in every paginated list response.
type gcpPage struct {
	NextPageToken string `json:"nextPageToken"`
}

func (p gcpPage) next() string { return p.NextPageToken }

// gcpListPages GETs a paginated collection, handing each page to `each`.
func gcpListPages[P interface{ next() string }](ctx context.Context, cfg gcpConfig, rawURL string, each func(P)) error {
	token := ""
	for {
		u := rawURL
		if token != "" {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "pageToken=" + url.QueryEscape(token)
		}
		var page P
		if err := gcpDo(ctx, cfg, http.MethodGet, u, nil, &page); err != nil {
			return err
		}
		each(page)
		if token = page.next(); token == "" {
			return nil
		}
	}
}

// gcpAPIError shapes a non-2xx response into an error carrying Google's own
// message. A 404 additionally wraps errGcpNotFound.
func gcpAPIError(rawURL string, resp *http.Response) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	var err error
	if body.Error.Message != "" {
		err = fmt.Errorf("%s: %s (HTTP %d %s)", apiHost(rawURL), body.Error.Message,
			resp.StatusCode, body.Error.Status)
	} else {
		err = fmt.Errorf("%s returned HTTP %d", apiHost(rawURL), resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", errGcpNotFound, err)
	}
	return err
}

func apiHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

// lastPathSegment returns the final element of a resource path.
func lastPathSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}
