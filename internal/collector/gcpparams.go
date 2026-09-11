package collector

import (
	"fmt"
	"strings"
)

// DefaultGcpDeploymentName is the Infrastructure Manager deployment (and, by
// the template's naming contract, the MIG) an install creates by default.
const DefaultGcpDeploymentName = "dbgorilla-collector"

// gceMetadataConfigLimit caps the base64 collector config carried in instance
// metadata (the per-value limit is 256KiB).
const gceMetadataConfigLimit = 245760

// gcpInputKeys and GcpSecretInputKeys together are the template's
// input-variable contract; TestGcpTemplateContract pins their union against
// variables.tf. They are split because the two maps GcpDeployInputs returns
// must never mix: the non-secret map is printable (dry runs), the secret map
// is merged into the request only at send time.
var gcpInputKeys = []string{
	"collector_config",
	"collector_image",
	"nat_subnet_cidr",
	"network",
	"region",
	"runtime_service_account",
	"stable_egress",
	"subnetwork",
}

// GcpSecretInputKeys are the template inputs that carry credentials. A dry
// run prints their presence, never their values.
var GcpSecretInputKeys = []string{"db_password", "instaclustr_api_key", "server_secret"}

// GcpRuntimeServiceAccountFor is the service account the template creates for
// the collector VM. The IAM database user derives from it before it exists.
func GcpRuntimeServiceAccountFor(deploymentName, project string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", deploymentName, project)
}

// GcpDatabaseUserFor is the database-side username for a service account under
// IAM auth: Postgres drops the .gserviceaccount.com suffix, MySQL keeps only
// the part before the @.
func GcpDatabaseUserFor(saEmail, engine string) string {
	if engine == "mysql" {
		local, _, _ := strings.Cut(saEmail, "@")
		return local
	}
	return strings.TrimSuffix(saEmail, ".gserviceaccount.com")
}

// GcpStackInput carries everything GcpDeployInputs needs.
type GcpStackInput struct {
	AgentID   string
	TenantID  string
	Image     string
	Endpoints Endpoints
	Targets   []GcpTarget
	// Network is the VPC the collector instance joins; Subnetwork is empty on
	// an auto-mode VPC.
	Network        string
	Subnetwork     string
	Region         string
	DeploymentName string
	Project        string
	ServerSecret   string
	// DBPassword may be empty when every target uses IAM auth. It rides its
	// own input, never the config document.
	DBPassword      string
	CommandsEnabled bool
	// Instaclustr source riding the gcp substrate: the READ-ONLY API key the
	// instance keeps for discovery (its own Secret Manager secret), and the
	// pre-rendered components (Targets stays empty — there is no Cloud SQL).
	InstaclustrKey string
	Components     []Component
	// Stable egress (template-owned subnetwork + Cloud NAT + reserved static
	// address): the collector's outbound IP never changes, which
	// IP-allowlist-gated databases require. NatSubnetCidr is the subnetwork's
	// range; Subnetwork is ignored when set, the template picks its own.
	StableEgress  bool
	NatSubnetCidr string
}

// GcpDeployInputs renders the template's input variables as two maps: the
// printable inputs, and the secrets. Secrets are kept out of the first map by
// construction — a dry run prints it wholesale, and a map that ever held a
// credential can't be proven clean by inspection (or by a taint analysis) —
// so the only place the two meet is the deploy request body.
func GcpDeployInputs(in GcpStackInput) (inputs, secrets map[string]string, err error) {
	var configTOML string
	if len(in.Components) > 0 {
		// Pre-rendered components (the instaclustr source): no Cloud SQL
		// discovery, no per-target render — the caller built the blocks.
		configTOML, err = componentsConfigTOML(in.AgentID, in.TenantID, in.Components, in.Endpoints, in.CommandsEnabled)
	} else {
		configTOML, err = GcpConfigTOML(in.AgentID, in.TenantID, in.Targets, in.Endpoints, in.CommandsEnabled)
	}
	if err != nil {
		return nil, nil, err
	}
	encoded, err := encodeConfigLimited(configTOML, gceMetadataConfigLimit, "a GCE metadata value")
	if err != nil {
		return nil, nil, err
	}
	stableEgress := "false"
	if in.StableEgress {
		stableEgress = "true"
	}
	inputs = map[string]string{
		"collector_config":        encoded,
		"collector_image":         in.Image,
		"nat_subnet_cidr":         in.NatSubnetCidr,
		"network":                 in.Network,
		"region":                  in.Region,
		"runtime_service_account": GcpRuntimeServiceAccountFor(in.DeploymentName, in.Project),
		"stable_egress":           stableEgress,
		"subnetwork":              in.Subnetwork,
	}
	secrets = map[string]string{
		"db_password":         in.DBPassword,
		"instaclustr_api_key": in.InstaclustrKey,
		"server_secret":       in.ServerSecret,
	}
	return inputs, secrets, nil
}
