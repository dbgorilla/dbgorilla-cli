package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrCollectorUnsupported is returned when the deployment has no managed
// collector API (the release line, or a backend predating v0_2).
var ErrCollectorUnsupported = errors.New("this deployment does not support the managed collector (needs a main-based backend)")

// CollectorCredentials is the response from POST /api/v0_2/collectors. The
// secret is returned exactly once. agent_id is the OAuth client_id; domain is
// the deployment domain for the collector's endpoints.
type CollectorCredentials struct {
	AgentID  string `json:"agent_id"`
	Secret   string `json:"secret"`
	TenantID string `json:"tenant_id"`
	Domain   string `json:"domain"`
	// Optional per-service endpoints (contract agreed with backend; populated
	// only for non-prod/self-hosted deployments). Empty -> the collector uses
	// its built-in production defaults.
	KeycloakBaseURL string `json:"keycloak_base_url,omitempty"`
	OtlpBaseURL     string `json:"otlp_base_url,omitempty"`
	OpampBaseURL    string `json:"opamp_base_url,omitempty"`
	// PreferredCollectorVersion is the collector version the deployment blesses
	// for this environment (e.g. "0.1.0"). Empty -> the CLI uses its built-in
	// default image. The CLI pins this version unless --image overrides it.
	PreferredCollectorVersion string `json:"preferred_collector_version,omitempty"`
}

// CollectorSupported probes whether the deployment exposes the v0_2 collector
// API. A 404 means unsupported (release line); any other reachable status
// (including auth challenges) means the route exists.
func (c *Client) CollectorSupported() (bool, error) {
	_, status, err := c.Get("/api/v0_2/collectors")
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		return false, nil
	}
	return status < http.StatusInternalServerError, nil
}

// ProvisionCollector mints a new collector identity. The caller's user token
// authorizes the mint; the backend (via the dbg-ingest bridge) creates the
// Keycloak client and returns its credentials.
func (c *Client) ProvisionCollector() (*CollectorCredentials, error) {
	body, status, err := c.Post("/api/v0_2/collectors", nil)
	if err != nil {
		return nil, fmt.Errorf("cannot provision collector: %w", err)
	}
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		return nil, ErrCollectorUnsupported
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("backend returned HTTP %d provisioning collector:\n%s", status, string(body))
	}
	var cc CollectorCredentials
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, fmt.Errorf("cannot parse collector credentials: %w", err)
	}
	if cc.AgentID == "" || cc.Secret == "" {
		return nil, fmt.Errorf("backend returned an incomplete collector credential response")
	}
	return &cc, nil
}

// CollectorStatus returns the live connection status for a collector. The
// shape is the dbg-ingest bridge StatusResponse, passed through; we surface the
// raw JSON plus a best-effort status string.
type CollectorStatus struct {
	Status string         `json:"status"`
	Raw    map[string]any `json:"-"`
}

// FetchCollectorStatus calls GET /api/v0_2/collectors/{id}/status. A 404 maps
// to (nil, nil) — the collector is not known to the control plane yet.
func (c *Client) FetchCollectorStatus(agentID string) (*CollectorStatus, error) {
	body, status, err := c.Get("/api/v0_2/collectors/" + agentID + "/status")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("backend returned HTTP %d fetching collector status:\n%s", status, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("cannot parse collector status: %w", err)
	}
	cs := &CollectorStatus{Raw: raw}
	if s, ok := raw["status"].(string); ok {
		cs.Status = s
	}
	return cs, nil
}

// DeleteCollector deprovisions a collector identity; its credentials stop
// working immediately. A 404 is treated as already-gone (idempotent).
func (c *Client) DeleteCollector(agentID string) error {
	body, status, err := c.Do(http.MethodDelete, "/api/v0_2/collectors/"+agentID, nil)
	if err != nil {
		return fmt.Errorf("cannot delete collector: %w", err)
	}
	if status == http.StatusNoContent || status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("backend returned HTTP %d deleting collector:\n%s", status, string(body))
}

// ListCollectors returns the tenant's collector agents. The bridge's AgentPage
// envelope key isn't pinned, so we accept the common shapes (items/agents/data)
// or a bare array and return the records as generic maps for display.
func (c *Client) ListCollectors() ([]map[string]any, error) {
	body, status, err := c.Get("/api/v0_2/collectors")
	if err != nil {
		return nil, fmt.Errorf("cannot list collectors: %w", err)
	}
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		return nil, ErrCollectorUnsupported
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("backend returned HTTP %d listing collectors:\n%s", status, string(body))
	}
	// Try an enveloped object first.
	var env struct {
		Items  []map[string]any `json:"items"`
		Agents []map[string]any `json:"agents"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		switch {
		case env.Items != nil:
			return env.Items, nil
		case env.Agents != nil:
			return env.Agents, nil
		case env.Data != nil:
			return env.Data, nil
		}
	}
	// Fall back to a bare array.
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("could not parse collector list response")
}
