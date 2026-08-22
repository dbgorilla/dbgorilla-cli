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
	"fmt"
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

	// AwsDBPasswordEnv is the password reference for the aws target. It differs
	// from DBPasswordEnv because the Fargate task definition names the variable
	// itself (fed from Secrets Manager), while the docker target names it in the
	// env-file it writes.
	AwsDBPasswordEnv = "DBG_DB_PASSWORD"

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
	AgentID      string `toml:"agent_id"`
	TenantID     string `toml:"tenant_id"`
	Secret       string `toml:"secret"`
	OpampBaseURL string `toml:"opamp_base_url,omitempty"`
	OtlpBaseURL  string `toml:"otlp_base_url,omitempty"`
	AuthBaseURL  string `toml:"auth_base_url,omitempty"`
}

// Component is one [[component]] to monitor.
type Component struct {
	Name   string `toml:"name"`
	Engine string `toml:"engine"`
	// Commands the control plane may run against this database (execute_query,
	// explain). Omitted means "inherit the global [commands] default".
	Commands []string `toml:"commands,omitempty"`
	Provider Provider `toml:"provider"`
	Auth     Auth     `toml:"auth"`
	Connect  Connect  `toml:"connect"`
}

// Provider is [component.provider]. self_hosted carries no extra fields; the
// aws_rds / aws_aurora providers add the region and the instance or cluster id
// (exactly one of the two); cnpg adds the Kubernetes namespace and
// CloudNativePG Cluster name.
type Provider struct {
	Type       string `toml:"type"`
	Region     string `toml:"region,omitempty"`
	InstanceID string `toml:"instance_id,omitempty"`
	ClusterID  string `toml:"cluster_id,omitempty"`
	RoleArn    string `toml:"role_arn,omitempty"`

	// cnpg. Namespace + Cluster are the whole of the component's identity: the
	// collector keys it as cnpg:{namespace}/{cluster}, deliberately excluding the
	// instance so a failover -- the event the operator exists to handle -- does
	// not re-key the component and detach its history.
	Namespace string `toml:"namespace,omitempty"`
	Cluster   string `toml:"cluster,omitempty"`

	Kubernetes *KubernetesConfig `toml:"kubernetes,omitempty"`
	Metrics    *MetricsConfig    `toml:"metrics,omitempty"`
}

// KubernetesConfig is [component.provider.kubernetes]. Mode decides what happens
// when the Kubernetes API is unreachable or RBAC was refused:
//
//	auto     probe at discovery; full mode if reachable, metrics-only if not
//	enabled  require it; fail loudly when the grant is missing
//	disabled never call the API; metrics-only, no permissions needed
//
// auto is the default, which is what makes a refused RBAC grant a degradation
// rather than a failed install -- and makes enabling full mode later a
// permission grant rather than a reinstall.
type KubernetesConfig struct {
	Mode string `toml:"mode"`
}

// MetricsConfig is [component.provider.metrics] -- the instance-manager scrape.
//
// Scheme/CACert/ServerName exist for metrics-only mode specifically. In full
// mode the provider reads .spec.monitoring.tls.enabled off the Cluster resource
// and configures the scrape itself. With no Kubernetes API there is no Cluster
// to read, so the one fact that decides plain HTTP versus HTTPS-with-a-verified
// name has to come from config -- otherwise the collector guesses, scrapes
// nothing, and an empty series set reads as a healthy cluster.
type MetricsConfig struct {
	Port int `toml:"port"`
	// Scheme is http (the CNPG 1.29/1.30 default) or https.
	Scheme string `toml:"scheme,omitempty"`
	// CACert is the in-container path to the cluster CA bundle, required when
	// Scheme is https.
	CACert string `toml:"ca_cert,omitempty"`
	// ServerName is the name the certificate is verified against. CNPG issues one
	// shared server certificate whose SANs cover only the three role services, so
	// this is <cluster>-rw even though the scrape dials a pod IP.
	ServerName string `toml:"server_name,omitempty"`
}

// Auth is [component.auth]. Password is a ${VAR} reference, never a literal, so
// it is omitted entirely for IAM auth.
type Auth struct {
	Method   string `toml:"method"`
	User     string `toml:"user"`
	Password string `toml:"password,omitempty"`
}

// Connect is [component.connect].
type Connect struct {
	Host      string   `toml:"host"`
	Port      int      `toml:"port"`
	Databases []string `toml:"databases,omitempty"`
	SSLMode   string   `toml:"ssl_mode"`
	// CACert is the bundle the server certificate is verified against; empty
	// means the OS trust store. RDS certificates are not publicly rooted, so an
	// AWS target verifying the server must name the bundle baked into the image.
	CACert string `toml:"ca_cert,omitempty"`
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
	OpampBaseURL string
	OtlpBaseURL  string
	AuthBaseURL  string
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
			AgentID:      agentID,
			TenantID:     tenantID,
			Secret:       "${" + SecretEnv + "}",
			OpampBaseURL: eps.OpampBaseURL,
			OtlpBaseURL:  eps.OtlpBaseURL,
			AuthBaseURL:  eps.AuthBaseURL,
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

// LoadConfig decodes an installed collector.toml back into a Config, so `dbg
// collector status` can recover the monitored target's connection details
// without re-prompting.
func LoadConfig(path string) (Config, error) {
	var c Config
	_, err := toml.DecodeFile(path, &c)
	return c, err
}

// ParseConfig decodes collector.toml text into a Config. The aws target uses it
// to read back the config it stored as a stack parameter, so an update can
// change the monitored databases while preserving the identity and endpoints
// the install minted.
func ParseConfig(s string) (Config, error) {
	var c Config
	_, err := toml.Decode(s, &c)
	return c, err
}

// StrictParseConfig is ParseConfig that refuses to silently drop keys it does
// not model. The aws target round-trips the stored config through Config on
// every update — parse, replace the components, re-render — so any key this
// build does not know about would be quietly deleted from a running collector's
// configuration. Failing tells the operator to upgrade instead.
//
// ParseConfig stays permissive: `encode-config` hands it hand-written files
// whose documented example exercises collector options beyond what this CLI
// generates, and merely encoding them must not require modelling them.
func StrictParseConfig(s string) (Config, error) {
	var c Config
	md, err := toml.Decode(s, &c)
	if err != nil {
		return c, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			keys = append(keys, k.String())
		}
		return c, fmt.Errorf("config contains %d setting(s) this version of dbg does not understand (%s). "+
			"Upgrade with 'dbg upgrade' and re-run, so the update preserves them",
			len(keys), strings.Join(keys, ", "))
	}
	return c, nil
}

// DialHost reverses the container host rewrite for a HOST-side connection: a
// config host of host.docker.internal (written so the in-container collector
// can reach a DB on the host) maps back to localhost for this CLI process. Any
// other host is returned unchanged.
func DialHost(configHost string) string {
	if strings.EqualFold(strings.Trim(configHost, "[]"), DockerHostInternal) {
		return "localhost"
	}
	return configHost
}

// HostDial returns the host:port a process on the host (i.e. this CLI) uses to
// reach the target, for the pre-install reachability check. This deliberately
// uses the original loopback host, not the rewritten container host.
func (t Target) HostDial() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}
