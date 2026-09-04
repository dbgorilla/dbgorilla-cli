package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// A fake Google transport, so every REST path in the gcp target is reachable
// without a project. Requests are matched on "METHOD /path" (the host is
// ignored — Cloud SQL, AlloyDB, Infrastructure Manager, Compute, Logging and
// Cloud Storage all share the one transport), and anything unmatched fails the
// test loudly rather than returning a plausible-looking empty result.

type gcpFakeResp struct {
	code int
	body string
}

type gcpFake struct {
	t         *testing.T
	responses map[string][]gcpFakeResp
	seen      map[string]int
	// calls records "METHOD /path?query" for every request, in order.
	calls []string
	// bodies records the raw request body per "METHOD /path", in order.
	bodies map[string][]string
}

func newGCPFake(t *testing.T) *gcpFake {
	t.Helper()
	return &gcpFake{
		t:         t,
		responses: map[string][]gcpFakeResp{},
		seen:      map[string]int{},
		bodies:    map[string][]string{},
	}
}

// on registers the response for a method + path.
func (f *gcpFake) on(method, path string, code int, body string) *gcpFake {
	f.responses[method+" "+path] = []gcpFakeResp{{code, body}}
	return f
}

// onSeq registers successive responses for repeated calls to one method +
// path. The final one repeats forever, which is what a polling loop needs.
func (f *gcpFake) onSeq(method, path string, resps ...gcpFakeResp) *gcpFake {
	f.responses[method+" "+path] = resps
	return f
}

// called reports how many times a method + path was invoked (query ignored).
func (f *gcpFake) called(method, path string) int {
	return f.seen[method+" "+path]
}

// mutations lists every non-GET call, so a test can assert nothing was touched.
func (f *gcpFake) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// lastBody returns the most recent request body sent to a method + path.
func (f *gcpFake) lastBody(method, path string) string {
	b := f.bodies[method+" "+path]
	if len(b) == 0 {
		return ""
	}
	return b[len(b)-1]
}

// lastCall returns the most recent "METHOD /path?query" matching a prefix.
func (f *gcpFake) lastCall(prefix string) string {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.calls[i], prefix) {
			return f.calls[i]
		}
	}
	return ""
}

func (f *gcpFake) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Path
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
	}
	f.calls = append(f.calls, key+"?"+req.URL.RawQuery)
	f.bodies[key] = append(f.bodies[key], string(raw))

	if f.seen[key] >= maxFakeCalls {
		f.t.Fatalf("%s called %d times — the stubbed responses never reach a terminal state, "+
			"so a poll loop is spinning forever. Give it a sequence ending in the state it waits for (onSeq).",
			key, f.seen[key])
	}
	resps, ok := f.responses[key]
	if !ok || len(resps) == 0 {
		f.t.Errorf("unstubbed Google API call: %s (stub it with .on(%q, %q, ...))", key, req.Method, req.URL.Path)
		return gcpResponse(http.StatusInternalServerError,
			`{"error":{"message":"not stubbed","status":"INTERNAL"}}`), nil
	}
	i := min(f.seen[key], len(resps)-1)
	f.seen[key]++
	return gcpResponse(resps[i].code, resps[i].body), nil
}

func gcpResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// stubGCP points every Google call this package makes at the fake, with a
// static token and a fixed project so nothing touches the environment. Polling
// runs at full speed.
func stubGCP(t *testing.T, f *gcpFake) {
	t.Helper()
	orig, origPoll := loadGCPConfig, gcpPollInterval
	loadGCPConfig = func(context.Context) (gcpConfig, error) {
		return gcpConfig{
			http:    &http.Client{Transport: f},
			tokens:  oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
			project: "test-project",
		}, nil
	}
	gcpPollInterval = 0
	t.Cleanup(func() { loadGCPConfig, gcpPollInterval = orig, origPoll })
}

// stubGCPConfigError makes credential resolution itself fail — the "no ADC /
// expired login" path every GCP command has to survive.
func stubGCPConfigError(t *testing.T, err error) {
	t.Helper()
	orig := loadGCPConfig
	loadGCPConfig = func(context.Context) (gcpConfig, error) { return gcpConfig{}, err }
	t.Cleanup(func() { loadGCPConfig = orig })
}

var errNoADC = errors.New("google: could not find default credentials")

// --- JSON fixtures ---------------------------------------------------------

// sqlInstancesJSON renders a Cloud SQL instances.list page.
func sqlInstancesJSON(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// sqlInstancesPageJSON renders a page that continues with nextPageToken.
func sqlInstancesPageJSON(next string, items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `],"nextPageToken":"` + next + `"}`
}

// sqlInstanceJSON renders one Cloud SQL instance: a Postgres 16 primary in
// us-central1 with a private IP, a PSA DNS name, IAM auth on and a CAS server
// CA — the fields the mapper reads. master names a primary to make this a
// replica.
func sqlInstanceJSON(name, version, master string) string {
	return fmt.Sprintf(`{
	  "name": %q, "region": "us-central1", "databaseVersion": %q, "masterInstanceName": %q,
	  "ipAddresses": [{"type":"PRIMARY","ipAddress":"34.1.2.3"},{"type":"PRIVATE","ipAddress":"10.0.0.4"}],
	  "dnsNames": [{"name":"abc.def.us-central1.sql-psa.goog."}],
	  "settings": {
	    "ipConfiguration": {"serverCaMode":"GOOGLE_MANAGED_CAS_CA","privateNetwork":"projects/p/global/networks/default"},
	    "databaseFlags": [{"name":"cloudsql.iam_authentication","value":"on"}]
	  }
	}`, name, version, master)
}

func deploymentJSON(state, detail string) string {
	return fmt.Sprintf(`{"name":"projects/p/locations/us-central1/deployments/dbg","state":%q,"stateDetail":%q}`, state, detail)
}

func operationJSON(name string, done bool, errMsg string) string {
	if errMsg != "" {
		return fmt.Sprintf(`{"name":%q,"done":true,"error":{"code":9,"message":%q}}`, name, errMsg)
	}
	return fmt.Sprintf(`{"name":%q,"done":%v}`, name, done)
}

const gcpNotFoundJSON = `{"error":{"code":404,"message":"Resource not found","status":"NOT_FOUND"}}`
const gcpDeniedJSON = `{"error":{"code":403,"message":"Permission config.deployments.get denied","status":"PERMISSION_DENIED"}}`
