package collector

import (
	"errors"
	"fmt"
	"strings"
)

// CloudNativePG (CNPG) runs PostgreSQL as Kubernetes pods. The collector runs
// in-cluster as a Helm release rather than as a container this CLI starts, so
// `dbg collector helm-values` does the two things Helm cannot -- mint the
// identity and render the config -- and hands back the chart values plus the
// install command. That mirrors `dbg collector encode-config`, which exists for
// the same reason on the CloudFormation path.
const (
	// CNPGProviderType is the [component.provider] discriminator.
	CNPGProviderType = "cnpg"

	// DefaultMetricsPort is the CNPG instance-manager metrics port. Plain HTTP
	// unless .spec.monitoring.tls.enabled is set on the Cluster.
	DefaultMetricsPort = 9187

	// DefaultChartRef is the OCI reference for the collector chart.
	DefaultChartRef = "oci://dbgorillapublic.azurecr.io/charts/dbg-collector"

	// DefaultReleaseName / DefaultReleaseNamespace are the Helm release's own
	// name and namespace -- where the COLLECTOR runs, not where the database is.
	DefaultReleaseName      = "dbg-collector"
	DefaultReleaseNamespace = "dbg-collector"

	// K8sModeAuto probes the Kubernetes API at discovery and silently degrades to
	// metrics-only when it is unreachable. It is the default because it turns a
	// refused RBAC grant into a degradation rather than a failed install.
	K8sModeAuto = "auto"
	// K8sModeEnabled requires the API and fails loudly without it.
	K8sModeEnabled = "enabled"
	// K8sModeDisabled never calls the API. Metrics-only, zero permissions.
	K8sModeDisabled = "disabled"
)

// K8sModes lists the accepted --k8s-mode values, in help order.
func K8sModes() []string { return []string{K8sModeAuto, K8sModeEnabled, K8sModeDisabled} }

// CNPGTarget describes one CloudNativePG cluster to monitor.
//
// Cluster is REQUIRED and deliberately not optional. The component's stable key
// is computed once per [[component]] stanza before discovery runs, so there is
// no path by which one stanza yields several components -- "omit cluster to
// discover every Cluster in the namespace" cannot work without a separate
// design. Requiring it here fails at the flag rather than at runtime.
type CNPGTarget struct {
	Name      string
	Namespace string
	Cluster   string
	Databases []string
	User      string
	SSLMode   string

	K8sMode     string
	MetricsPort int
	// MetricsTLS reflects .spec.monitoring.tls.enabled. It only has to be
	// supplied when Kubernetes access is off, because with access the provider
	// reads it from the Cluster resource itself.
	MetricsTLS   bool
	MetricsCA    string
	CommandsOn   bool
	TopologyEach string
}

// ErrClusterRequired is returned when --cluster is missing.
var ErrClusterRequired = errors.New("--cluster is required: the component's identity is fixed before " +
	"discovery runs, so one config entry monitors exactly one CNPG cluster. " +
	"Repeat the command per cluster, or see the docs for monitoring several from one collector")

// RoleServiceHost returns the read-write role service the SQL connection uses:
// <cluster>-rw.<namespace>.
//
// SQL never targets a pod. CNPG issues ONE server certificate shared by every
// instance, and its SANs cover only the three role services -- no pod names, no
// IP addresses -- so a per-pod SQL connection cannot pass the verify-full
// hostname check this CLI defaults to. Per-instance state comes from the
// primary's metrics port instead, which can override the verified name and a
// SQL connection cannot.
func (t CNPGTarget) RoleServiceHost() string {
	return fmt.Sprintf("%s-rw.%s", t.Cluster, t.Namespace)
}

// StableKey is the component identity the collector derives, reproduced here so
// the rendered config can be explained to the operator. No instance appears in
// it: a failover would otherwise re-key the component and detach all its history
// with nothing having gone wrong.
func (t CNPGTarget) StableKey() string {
	return fmt.Sprintf("cnpg:%s/%s", t.Namespace, t.Cluster)
}

// Validate reports the first problem with the target, with the fix in the error.
func (t CNPGTarget) Validate() error {
	if strings.TrimSpace(t.Namespace) == "" {
		return errors.New("--namespace is required: it names the namespace the CNPG Cluster runs in")
	}
	if strings.TrimSpace(t.Cluster) == "" {
		return ErrClusterRequired
	}
	switch t.K8sMode {
	case K8sModeAuto, K8sModeEnabled, K8sModeDisabled:
	default:
		return fmt.Errorf("unknown --k8s-mode %q (expected %s)", t.K8sMode, strings.Join(K8sModes(), ", "))
	}
	if t.MetricsPort <= 0 || t.MetricsPort > 65535 {
		return fmt.Errorf("--metrics-port %d is not a valid port", t.MetricsPort)
	}
	if t.MetricsTLS && strings.TrimSpace(t.MetricsCA) == "" {
		return errors.New("--metrics-tls needs --metrics-ca: the scrape verifies the CNPG server " +
			"certificate against the cluster CA, and skipping verification is not offered")
	}
	if strings.TrimSpace(t.User) == "" {
		return errors.New("--db-user is required: the read-only role the collector connects as")
	}
	return nil
}

// BuildCNPG assembles the collector config for a CNPG component. The secret and
// the database password are ${ENV} references, never literals -- the rendered
// TOML is printed to a terminal and pasted into a chart value, so it must be
// safe to look at.
func BuildCNPG(agentID, tenantID string, t CNPGTarget, eps Endpoints) Config {
	sslMode := t.SSLMode
	if sslMode == "" {
		sslMode = "verify-full"
	}
	metrics := &MetricsConfig{Port: t.MetricsPort}
	if t.MetricsTLS {
		metrics.Scheme = "https"
		metrics.CACert = t.MetricsCA
		// CNPG's shared certificate lists the role services and nothing else, so
		// the scrape dials the pod IP but verifies this name.
		metrics.ServerName = fmt.Sprintf("%s-rw", t.Cluster)
	}
	interval := t.TopologyEach
	if interval == "" {
		interval = "60s"
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
			Name:   orDefault(t.Name, t.Cluster),
			Engine: "postgres",
			Provider: Provider{
				Type:       CNPGProviderType,
				Namespace:  t.Namespace,
				Cluster:    t.Cluster,
				Kubernetes: &KubernetesConfig{Mode: t.K8sMode},
				Metrics:    metrics,
			},
			Auth: Auth{
				Method:   "password",
				User:     t.User,
				Password: "${" + DBPasswordEnv + "}",
			},
			Connect: Connect{
				Host:      t.RoleServiceHost(),
				Port:      5432,
				Databases: t.Databases,
				SSLMode:   sslMode,
			},
		}},
		Topology: Topology{Interval: interval},
		Commands: Commands{Enabled: t.CommandsOn},
	}
}

// MetricsOnly reports whether this target will never reach the Kubernetes API.
// Only `disabled` is certain: `auto` degrades at runtime depending on whether
// the grant exists, which this CLI cannot know.
func (t CNPGTarget) MetricsOnly() bool { return t.K8sMode == K8sModeDisabled }

// DegradedCapabilities lists what a metrics-only install cannot collect, for the
// warning `helm-values` prints before it mints anything.
//
// This exists because the mode is chosen at a command line and its cost is
// invisible afterwards: nothing errors, the collector reports healthy, and the
// dashboard simply omits the sections it has no data for. The person choosing
// deserves to be told once, at the moment of choosing.
func DegradedCapabilities() []string {
	return []string{
		"backup state -- whether backups are running at all, and when the last one succeeded",
		"WAL archiving state -- so a stalled archiver, which silently degrades recoverability, is invisible",
		"failover and switchover history, and operator-detected primary faults",
	}
}
