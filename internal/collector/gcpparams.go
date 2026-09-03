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

// gcpInputKeys is the template's input-variable contract; TestGcpTemplateContract
// pins it against variables.tf.
var gcpInputKeys = []string{
	"collector_config",
	"collector_image",
	"db_password",
	"network",
	"region",
	"runtime_service_account",
	"server_secret",
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
	ServerSecret   string
	// DBPassword may be empty when every target uses IAM auth. It rides its
	// own input, never the config document.
	DBPassword      string
	CommandsEnabled bool
}

// GcpDeployInputs renders the template's input variables.
func GcpDeployInputs(in GcpStackInput) (map[string]string, error) {
	configTOML, err := GcpConfigTOML(in.AgentID, in.TenantID, in.Targets, in.Endpoints, in.CommandsEnabled)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeConfigLimited(configTOML, gceMetadataConfigLimit, "a GCE metadata value")
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"collector_config":        encoded,
		"collector_image":         in.Image,
		"db_password":             in.DBPassword,
		"network":                 in.Network,
		"region":                  in.Region,
		"runtime_service_account": GcpRuntimeServiceAccountFor(in.DeploymentName, in.Project),
		"server_secret":           in.ServerSecret,
		"subnetwork":              in.Subnetwork,
	}, nil
}
