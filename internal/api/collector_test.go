package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// --- CollectorSupported ---------------------------------------------------

func TestCollectorSupported(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantResult bool
	}{
		{"200 route exists", http.StatusOK, true},
		{"401 auth challenge still means route exists", http.StatusUnauthorized, true},
		{"403 forbidden still means route exists", http.StatusForbidden, true},
		{"404 release line has no collector API", http.StatusNotFound, false},
		{"501 not implemented", http.StatusNotImplemented, false},
		{"500 upstream error is not 'supported'", http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticAuth(t)
			srv := serverReturning(t, tc.status, "")
			got, err := NewClient(srv.URL).CollectorSupported()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantResult {
				t.Fatalf("status %d: got supported=%v want %v", tc.status, got, tc.wantResult)
			}
		})
	}
}

func TestCollectorSupportedTransportError(t *testing.T) {
	hermeticAuth(t)
	_, err := NewClient(deadServerURL(t)).CollectorSupported()
	if err == nil {
		t.Fatal("expected transport error")
	}
}

// --- ProvisionCollector ---------------------------------------------------

func TestProvisionCollectorSuccess(t *testing.T) {
	body := `{
		"agent_id":"agent-1","secret":"s3cr3t","tenant_id":"t-1","domain":"deploy.example.com",
		"auth_base_url":"https://auth.example.com","otlp_base_url":"https://otlp.example.com",
		"opamp_base_url":"https://opamp.example.com","preferred_collector_version":"0.1.0"
	}`
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		hermeticAuth(t)
		srv := serverReturning(t, status, body)
		cc, err := NewClient(srv.URL).ProvisionCollector()
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if cc.AgentID != "agent-1" || cc.Secret != "s3cr3t" || cc.TenantID != "t-1" {
			t.Fatalf("core creds not parsed: %+v", cc)
		}
		if cc.Domain != "deploy.example.com" {
			t.Fatalf("domain not parsed: %q", cc.Domain)
		}
		if cc.AuthHost() != "https://auth.example.com" ||
			cc.OtlpBaseURL != "https://otlp.example.com" ||
			cc.OpampBaseURL != "https://opamp.example.com" {
			t.Fatalf("optional endpoints not parsed: %+v", cc)
		}
		if cc.PreferredCollectorVersion != "0.1.0" {
			t.Fatalf("preferred version not parsed: %q", cc.PreferredCollectorVersion)
		}
	}
}

func TestCollectorCredentials_AuthHost(t *testing.T) {
	// auth_base_url wins when present.
	cc := CollectorCredentials{AuthBaseURL: "https://auth", KeycloakBaseURL: "https://kc"}
	if got := cc.AuthHost(); got != "https://auth" {
		t.Fatalf("AuthHost() = %q, want auth_base_url", got)
	}
	// Falls back to the deprecated keycloak_base_url for older deployments.
	legacy := CollectorCredentials{KeycloakBaseURL: "https://kc"}
	if got := legacy.AuthHost(); got != "https://kc" {
		t.Fatalf("AuthHost() fallback = %q, want keycloak_base_url", got)
	}
	// Empty when neither is set (collector uses its built-in default).
	if got := (CollectorCredentials{}).AuthHost(); got != "" {
		t.Fatalf("AuthHost() = %q, want empty", got)
	}
}

func TestProvisionCollectorUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		hermeticAuth(t)
		srv := serverReturning(t, status, "")
		_, err := NewClient(srv.URL).ProvisionCollector()
		if !errors.Is(err, ErrCollectorUnsupported) {
			t.Fatalf("status %d: expected ErrCollectorUnsupported, got %v", status, err)
		}
	}
}

func TestProvisionCollectorServerError(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusInternalServerError, "kaboom")
	_, err := NewClient(srv.URL).ProvisionCollector()
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("expected HTTP 500 error carrying body, got %v", err)
	}
}

func TestProvisionCollectorBadJSON(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusCreated, "not json")
	_, err := NewClient(srv.URL).ProvisionCollector()
	if err == nil || !strings.Contains(err.Error(), "cannot parse collector credentials") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestProvisionCollectorIncompleteCreds(t *testing.T) {
	hermeticAuth(t)
	// Valid JSON but missing the secret -> incomplete.
	srv := serverReturning(t, http.StatusCreated, `{"agent_id":"a"}`)
	_, err := NewClient(srv.URL).ProvisionCollector()
	if err == nil || !strings.Contains(err.Error(), "incomplete collector credential response") {
		t.Fatalf("expected incomplete-creds error, got %v", err)
	}
}

func TestProvisionCollectorTransportError(t *testing.T) {
	hermeticAuth(t)
	_, err := NewClient(deadServerURL(t)).ProvisionCollector()
	if err == nil || !strings.Contains(err.Error(), "cannot provision collector") {
		t.Fatalf("expected provision transport error, got %v", err)
	}
}

// --- FetchCollectorStatus -------------------------------------------------

func TestFetchCollectorStatusSuccess(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, `{"status":"connected","last_seen":"now"}`)
	cs, err := NewClient(srv.URL).FetchCollectorStatus("agent-1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cs == nil || cs.Status != "connected" {
		t.Fatalf("expected connected status, got %+v", cs)
	}
	if cs.Raw["last_seen"] != "now" {
		t.Fatalf("raw payload not preserved: %+v", cs.Raw)
	}
}

// A payload without a string "status" key still parses; Status stays empty and
// the raw map is preserved.
func TestFetchCollectorStatusNoStatusField(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, `{"other":123}`)
	cs, err := NewClient(srv.URL).FetchCollectorStatus("agent-1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cs == nil || cs.Status != "" {
		t.Fatalf("expected empty status, got %+v", cs)
	}
	if _, ok := cs.Raw["other"]; !ok {
		t.Fatalf("raw payload not preserved: %+v", cs.Raw)
	}
}

// 404 is the "not known to the control plane yet" sentinel -> (nil, nil).
func TestFetchCollectorStatusNotFound(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusNotFound, "")
	cs, err := NewClient(srv.URL).FetchCollectorStatus("agent-1")
	if err != nil || cs != nil {
		t.Fatalf("expected (nil,nil) for 404, got cs=%+v err=%v", cs, err)
	}
}

func TestFetchCollectorStatusServerError(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusInternalServerError, "down")
	_, err := NewClient(srv.URL).FetchCollectorStatus("agent-1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestFetchCollectorStatusBadJSON(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, "not json")
	_, err := NewClient(srv.URL).FetchCollectorStatus("agent-1")
	if err == nil || !strings.Contains(err.Error(), "cannot parse collector status") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestFetchCollectorStatusTransportError(t *testing.T) {
	hermeticAuth(t)
	_, err := NewClient(deadServerURL(t)).FetchCollectorStatus("agent-1")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

// --- DeleteCollector ------------------------------------------------------

func TestDeleteCollectorIdempotentStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusOK, http.StatusNotFound} {
		hermeticAuth(t)
		srv := serverReturning(t, status, "")
		if err := NewClient(srv.URL).DeleteCollector("agent-1"); err != nil {
			t.Fatalf("status %d: expected success, got %v", status, err)
		}
	}
}

func TestDeleteCollectorServerError(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusInternalServerError, "nope")
	err := NewClient(srv.URL).DeleteCollector("agent-1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestDeleteCollectorTransportError(t *testing.T) {
	hermeticAuth(t)
	err := NewClient(deadServerURL(t)).DeleteCollector("agent-1")
	if err == nil || !strings.Contains(err.Error(), "cannot delete collector") {
		t.Fatalf("expected delete transport error, got %v", err)
	}
}

// --- ListCollectors -------------------------------------------------------

func TestListCollectorsEnvelopeAndBareShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // expected value of the single record's "id" field
	}{
		{"items envelope", `{"items":[{"id":"i1"}]}`, "i1"},
		{"agents envelope", `{"agents":[{"id":"a1"}]}`, "a1"},
		{"data envelope", `{"data":[{"id":"d1"}]}`, "d1"},
		{"bare array", `[{"id":"b1"}]`, "b1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticAuth(t)
			srv := serverReturning(t, http.StatusOK, tc.body)
			list, err := NewClient(srv.URL).ListCollectors()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list) != 1 || list[0]["id"] != tc.want {
				t.Fatalf("expected single record id=%q, got %+v", tc.want, list)
			}
		})
	}
}

func TestListCollectorsEmptyArray(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, `[]`)
	list, err := NewClient(srv.URL).ListCollectors()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}

func TestListCollectorsUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		hermeticAuth(t)
		srv := serverReturning(t, status, "")
		_, err := NewClient(srv.URL).ListCollectors()
		if !errors.Is(err, ErrCollectorUnsupported) {
			t.Fatalf("status %d: expected ErrCollectorUnsupported, got %v", status, err)
		}
	}
}

func TestListCollectorsServerError(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusInternalServerError, "boom")
	_, err := NewClient(srv.URL).ListCollectors()
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

// A response that is neither an accepted envelope nor a bare array of objects
// (here: a JSON string) is a hard parse failure.
func TestListCollectorsUnparseable(t *testing.T) {
	hermeticAuth(t)
	srv := serverReturning(t, http.StatusOK, `"totally-unexpected"`)
	_, err := NewClient(srv.URL).ListCollectors()
	if err == nil || !strings.Contains(err.Error(), "could not parse collector list response") {
		t.Fatalf("expected list parse error, got %v", err)
	}
}

func TestListCollectorsTransportError(t *testing.T) {
	hermeticAuth(t)
	_, err := NewClient(deadServerURL(t)).ListCollectors()
	if err == nil || !strings.Contains(err.Error(), "cannot list collectors") {
		t.Fatalf("expected list transport error, got %v", err)
	}
}

func TestHTTPError_401MapsToSessionExpired(t *testing.T) {
	if err := httpError("provisioning collector", http.StatusUnauthorized, []byte(`{"detail":"Invalid token"}`)); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("401 should map to ErrSessionExpired, got: %v", err)
	}
	// Other statuses keep the detailed message and are not session-expired.
	err := httpError("provisioning collector", http.StatusInternalServerError, []byte("boom"))
	if errors.Is(err, ErrSessionExpired) {
		t.Error("500 should not be treated as session-expired")
	}
	if err == nil || !contains(err.Error(), "500") || !contains(err.Error(), "boom") {
		t.Errorf("non-401 error should include status + body, got: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
