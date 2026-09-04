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
	"network",
	"region",
	"runtime_service_account",
	"subnetwork",
}

// GcpSecretInputKeys are the template inputs that carry credentials. A dry
// run prints their presence, never their values.
var GcpSecretInputKeys = []string{"db_password", "server_secret"}

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
}

// GcpDeployInputs renders the template's input variables as two maps: the
// printable inputs, and the secrets. Secrets are kept out of the first map by
// construction — a dry run prints it wholesale, and a map that ever held a
// credential can't be proven clean by inspection (or by a taint analysis) —
// so the only place the two meet is the deploy request body.
func GcpDeployInputs(in GcpStackInput) (inputs, secrets map[string]string, err error) {
	configTOML, err := GcpConfigTOML(in.AgentID, in.TenantID, in.Targets, in.Endpoints, in.CommandsEnabled)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := encodeConfigLimited(configTOML, gceMetadataConfigLimit, "a GCE metadata value")
	if err != nil {
		return nil, nil, err
	}
	inputs = map[string]string{
		"collector_config":        encoded,
		"collector_image":         in.Image,
		"network":                 in.Network,
		"region":                  in.Region,
		"runtime_service_account": GcpRuntimeServiceAccountFor(in.DeploymentName, in.Project),
		"subnetwork":              in.Subnetwork,
	}
	secrets = map[string]string{
		"db_password":   in.DBPassword,
		"server_secret": in.ServerSecret,
	}
	return inputs, secrets, nil
}
