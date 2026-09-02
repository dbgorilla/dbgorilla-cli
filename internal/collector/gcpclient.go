package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GCP access uses Application Default Credentials via golang.org/x/oauth2/google
// rather than shelling out to `gcloud`, so no external binary is required. ADC
// resolves from the same chain gcloud uses — GOOGLE_APPLICATION_CREDENTIALS, the
// gcloud ADC file, and instance metadata — so the customer's own credentials are
// reused and nothing sensitive passes through this tool.
//
// Dependency note (per the repo rule): x/oauth2 is the only addition. The full
// Google Cloud SDK would pull dozens of modules to make what is a handful of
// documented REST calls; the collector itself talks to these APIs the same way.

// gcpConfig is what every GCP call in this package builds from: an
// authenticated HTTP client, the raw token source (tokeninfo needs the token
// itself), and the credentials' default project.
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

// loadGCPConfig resolves ADC. A package var rather than a plain func so tests
// can substitute a config whose HTTP client answers from a fixture instead of
// the network — without it, every GCP code path is only reachable against a
// live project. Mirrors loadAWSConfig.
var loadGCPConfig = loadGCPConfigDefault

// loadGCPConfigDefault resolves ADC once (credential + project resolution).
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

// gcpCredsErr wraps a credential-resolution failure with the remediation, so
// every GCP entry point tells the operator the same next step.
func gcpCredsErr(err error) error {
	return fmt.Errorf("could not load Google Cloud credentials "+
		"(run 'gcloud auth application-default login', or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
}

// GcpAvailable returns nil when Google Cloud credentials resolve and mint a
// working token. Mirrors AwsAvailable / DockerAvailable.
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

// GcpIdentity returns the ADC principal (its email when the token carries
// one), for display during preflight — the install must name who it will act
// as before it acts.
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
		// Service-account tokens without the email scope still work; say so
		// rather than printing an empty identity.
		return "authenticated (the credential reports no email)", nil
	}
	return email, nil
}

// GcpProject returns the project the install will act on: the ADC default.
// Callers pass an explicit --project through instead when the flag is set.
func GcpProject() (string, error) {
	cfg, err := loadGCPConfig(context.Background())
	if err != nil {
		return "", gcpCredsErr(err)
	}
	if cfg.project == "" {
		return "", errors.New("the Google Cloud credentials name no project — " +
			"pass --project, or set one with 'gcloud config set project'")
	}
	return cfg.project, nil
}

// gcpTokenEmail validates the current token against Google's tokeninfo
// endpoint and returns the principal email when present. The token travels in
// a POST form body, never in a URL that proxies or logs could retain.
func gcpTokenEmail(ctx context.Context, cfg gcpConfig) (string, error) {
	tok, err := cfg.tokens.Token()
	if err != nil {
		return "", err
	}
	form := url.Values{"access_token": {tok.AccessToken}}
	resp, err := gcpSend(ctx, cfg, http.MethodPost, "https://oauth2.googleapis.com/tokeninfo",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// errGcpNotFound tags a 404, so callers for whom "absent" is a normal answer
// (a deployment that was never created) can errors.Is it instead of parsing.
var errGcpNotFound = errors.New("not found")

// gcpSend issues one authenticated request and returns the response when it is
// 2xx. Anything else becomes an error carrying Google's own message, which
// names the missing permission or resource better than a bare status would.
// The caller closes the body.
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
		defer resp.Body.Close()
		return nil, gcpAPIError(rawURL, resp)
	}
	return resp, nil
}

// gcpDo is gcpSend for the JSON APIs: body (nil for none) is marshalled, and
// the response decoded into out (nil to discard it). Every Cloud SQL, AlloyDB,
// Infrastructure Manager, Compute and Logging call in this package goes
// through here.
func gcpDo(ctx context.Context, cfg gcpConfig, method, rawURL string, body, out any) error {
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
	defer resp.Body.Close()
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

// gcpListPages GETs a paginated collection, handing each page to `each` until
// the API stops returning a page token. Skipping this and reading one page
// would silently hide every database or log line past the first.
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

// gcpAPIError shapes a non-2xx API response into an error with Google's own
// message extracted (it usually names the fix: the missing role, the exact
// resource, the API to enable). A 404 additionally wraps errGcpNotFound.
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

// lastPathSegment returns the final element of a resource path
// (projects/p/locations/l/clusters/NAME → NAME).
func lastPathSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}
