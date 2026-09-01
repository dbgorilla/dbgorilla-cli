package collector

import (
	"context"
	"encoding/json"
	"fmt"
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

// GcpAvailable returns nil when Google Cloud credentials resolve and mint a
// working token. Mirrors AwsAvailable / DockerAvailable.
func GcpAvailable() error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return fmt.Errorf("could not load Google Cloud credentials "+
			"(run 'gcloud auth application-default login', or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
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
		return "", fmt.Errorf("could not load Google Cloud credentials "+
			"(run 'gcloud auth application-default login', or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
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
		return "", fmt.Errorf("could not load Google Cloud credentials "+
			"(run 'gcloud auth application-default login', or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
	}
	if cfg.project == "" {
		return "", fmt.Errorf("the Google Cloud credentials name no project — " +
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/tokeninfo",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cfg.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tokeninfo rejected the credential (HTTP %d)", resp.StatusCode)
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// gcpGetJSON performs an authenticated GET and decodes the JSON response into
// out. Non-2xx responses become errors carrying the API's own message, which
// names the missing permission or resource better than a bare status would.
func gcpGetJSON(ctx context.Context, cfg gcpConfig, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := cfg.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcpAPIError(rawURL, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// gcpAPIError shapes a non-2xx API response into an error with Google's own
// message extracted (it usually names the fix: the missing role, the exact
// resource, the API to enable).
func gcpAPIError(rawURL string, resp *http.Response) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Message != "" {
		return fmt.Errorf("%s: %s (HTTP %d %s)", apiHost(rawURL), body.Error.Message,
			resp.StatusCode, body.Error.Status)
	}
	return fmt.Errorf("%s returned HTTP %d", apiHost(rawURL), resp.StatusCode)
}

func apiHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}
