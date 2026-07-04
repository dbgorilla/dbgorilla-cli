// Package collector renders the external dbg-collector's config, manages its
// Docker lifecycle, and persists local state so `dbg collector` can install,
// inspect, and remove a collector that monitors a developer's local Postgres.
//
// The collector itself is the Rust dbg-collector image; this package never
// talks to a database or the control plane directly. It only prepares config +
// secrets and drives Docker. Secrets are referenced in collector.toml as
// ${ENV} placeholders and supplied to the container via a 0600 env-file, never
// inlined into the TOML or onto the docker argv.
package collector

import (
	"bytes"
	"net"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Env var names the rendered collector.toml references and the env-file
// supplies. The collector expands ${VAR} references at load time.
const (
	SecretEnv     = "DBG_SERVER_SECRET"
	DBPasswordEnv = "COLLECTOR_DB_PASSWORD"

	// DockerHostInternal is the hostname that resolves to the Docker host from
	// inside a container (native on Docker Desktop; on Linux we add an
	// --add-host mapping to host-gateway).
	DockerHostInternal = "host.docker.internal"
)

// Config mirrors the dbg-collector collector.toml schema. Only the subset this
// CLI generates is modelled (postgres / self_hosted / password).
type Config struct {
	Dbgorilla Dbgorilla   `toml:"dbgorilla"`
	Component []Component `toml:"component"`
	Topology  Topology    `toml:"topology"`
	Commands  Commands    `toml:"commands"`
}

// Dbgorilla is the [dbgorilla] block: identity plus optional endpoint
// overrides. Empty *_base_url fields fall back to the collector's built-in
// production defaults, so local-dev-against-prod needs none of them.
type Dbgorilla struct {
	AgentID         string `toml:"agent_id"`
	TenantID        string `toml:"tenant_id"`
	Secret          string `toml:"secret"`
	OpampBaseURL    string `toml:"opamp_base_url,omitempty"`
	OtlpBaseURL     string `toml:"otlp_base_url,omitempty"`
	KeycloakBaseURL string `toml:"keycloak_base_url,omitempty"`
}

// Component is one [[component]] to monitor.
type Component struct {
	Name     string   `toml:"name"`
	Engine   string   `toml:"engine"`
	Provider Provider `toml:"provider"`
	Auth     Auth     `toml:"auth"`
	Connect  Connect  `toml:"connect"`
}

// Provider is [component.provider]. self_hosted carries no extra fields.
type Provider struct {
	Type string `toml:"type"`
}

// Auth is [component.auth].
type Auth struct {
	Method   string `toml:"method"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// Connect is [component.connect].
type Connect struct {
	Host      string   `toml:"host"`
	Port      int      `toml:"port"`
	Databases []string `toml:"databases,omitempty"`
	SSLMode   string   `toml:"ssl_mode"`
}

// Topology is [topology].
type Topology struct {
	Interval string `toml:"interval"`
}

// Commands is [commands].
type Commands struct {
	Enabled bool `toml:"enabled"`
}

// Target describes one local database the developer wants monitored.
type Target struct {
	Name      string
	Host      string
	Port      int
	Databases []string
	User      string
	SSLMode   string
}

// Endpoints carries optional explicit endpoint overrides (Phase 1: from the
// provisioning response; Phase 2: from the .well-known discovery document).
// Leave fields empty to use the collector's production defaults.
type Endpoints struct {
	OpampBaseURL    string
	OtlpBaseURL     string
	KeycloakBaseURL string
}

// IsLoopback reports whether host refers to the local loopback interface, in
// which case it must be rewritten to host.docker.internal for the containerized
// collector to reach a database running on the host.
func IsLoopback(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// Build assembles a Config from the minted identity, the target, and optional
// endpoint overrides. A loopback target host is rewritten to
// host.docker.internal so the in-container collector reaches the host's DB.
func Build(agentID, tenantID string, target Target, eps Endpoints) Config {
	connectHost := target.Host
	if IsLoopback(target.Host) {
		connectHost = DockerHostInternal
	}
	sslMode := target.SSLMode
	if sslMode == "" {
		sslMode = "verify-full"
	}
	return Config{
		Dbgorilla: Dbgorilla{
			AgentID:         agentID,
			TenantID:        tenantID,
			Secret:          "${" + SecretEnv + "}",
			OpampBaseURL:    eps.OpampBaseURL,
			OtlpBaseURL:     eps.OtlpBaseURL,
			KeycloakBaseURL: eps.KeycloakBaseURL,
		},
		Component: []Component{{
			Name:     target.Name,
			Engine:   "postgres",
			Provider: Provider{Type: "self_hosted"},
			Auth: Auth{
				Method:   "password",
				User:     target.User,
				Password: "${" + DBPasswordEnv + "}",
			},
			Connect: Connect{
				Host:      connectHost,
				Port:      target.Port,
				Databases: target.Databases,
				SSLMode:   sslMode,
			},
		}},
		Topology: Topology{Interval: "60s"},
		Commands: Commands{Enabled: false},
	}
}

// Render serializes the Config to collector.toml text.
func (c Config) Render() (string, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// HostDial returns the host:port a process on the host (i.e. this CLI) uses to
// reach the target, for the pre-install reachability check. This deliberately
// uses the original loopback host, not the rewritten container host.
func (t Target) HostDial() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}
