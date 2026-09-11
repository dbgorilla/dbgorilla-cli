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

// gcpInputKeys is the template's whole input-variable contract;
// TestGcpTemplateContract pins it against variables.tf. No input carries a
// credential: the CLI writes the three secrets to Secret Manager itself
// (EnsureGcpSecrets, by the template's naming contract) and the template only
// grants the collector's service account read access — so the map is
// printable, and nothing secret ever reaches Infrastructure Manager.
var gcpInputKeys = []string{
	"collector_config",
	"collector_image",
	"database_roles",
	"nat_subnet_cidr",
	"network",
	"region",
	"runtime_service_account",
	"stable_egress",
	"subnetwork",
}

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
	// Credentials never ride the inputs: the CLI writes them to Secret
	// Manager (EnsureGcpSecrets) and the config document references them as
	// $${ENV} placeholders the boot script resolves.
	CommandsEnabled bool
	// Instaclustr source riding the gcp substrate: the pre-rendered
	// components (Targets stays empty — there is no Cloud SQL).
	Components []Component
	// Stable egress (template-owned subnetwork + Cloud NAT + reserved static
	// address): the collector's outbound IP never changes, which
	// IP-allowlist-gated databases require. NatSubnetCidr is the subnetwork's
	// range; Subnetwork is ignored when set, the template picks its own.
	StableEgress  bool
	NatSubnetCidr string
}

// GcpDeployInputs renders the template's input variables. The map is
// printable by construction — GcpStackInput carries no credential, so no code
// path can put one here; the secrets travel through EnsureGcpSecrets instead.
func GcpDeployInputs(in GcpStackInput) (inputs map[string]string, err error) {
	var configTOML string
	if len(in.Components) > 0 {
		// Pre-rendered components (the instaclustr source): no Cloud SQL
		// discovery, no per-target render — the caller built the blocks.
		configTOML, err = componentsConfigTOML(in.AgentID, in.TenantID, in.Components, in.Endpoints, in.CommandsEnabled)
	} else {
		configTOML, err = GcpConfigTOML(in.AgentID, in.TenantID, in.Targets, in.Endpoints, in.CommandsEnabled)
	}
	if err != nil {
		return nil, err
	}
	encoded, err := encodeConfigLimited(configTOML, gceMetadataConfigLimit, "a GCE metadata value")
	if err != nil {
		return nil, err
	}
	stableEgress := "false"
	if in.StableEgress {
		stableEgress = "true"
	}
	// The Cloud SQL / AlloyDB project-wide roles only make sense when the
	// target IS a Google-managed database; pre-rendered components (the
	// instaclustr source) monitor something else entirely.
	databaseRoles := "true"
	if len(in.Components) > 0 {
		databaseRoles = "false"
	}
	inputs = map[string]string{
		"collector_config":        encoded,
		"collector_image":         in.Image,
		"database_roles":          databaseRoles,
		"nat_subnet_cidr":         in.NatSubnetCidr,
		"network":                 in.Network,
		"region":                  in.Region,
		"runtime_service_account": GcpRuntimeServiceAccountFor(in.DeploymentName, in.Project),
		"stable_egress":           stableEgress,
		"subnetwork":              in.Subnetwork,
	}
	return inputs, nil
}
