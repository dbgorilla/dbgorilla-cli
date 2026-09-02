package collector

import (
	"fmt"
	"strings"
)

// DefaultGcpDeploymentName is the Infrastructure Manager deployment (and, by
// the template's naming contract, the MIG) an install creates by default.
const DefaultGcpDeploymentName = "dbgorilla-collector"

// gcpInputKeys is the template's input-variable contract — the gcp analogue of
// fargateParamKeys. Every deploy sends exactly these; the template versions on
// changes to this set, and TestGcpTemplateContract pins the two together.
var gcpInputKeys = []string{
	"collector_config",
	"collector_image",
	"db_password",
	"network",
	"region",
	"runtime_service_account",
	"server_secret",
}

// GcpRuntimeServiceAccountFor is the service account the template creates for
// the collector VM — a naming contract shared with the template. The install
// computes it upfront because the IAM database user (gcp_iam auth) is derived
// from it, and the config referencing that user is rendered before the account
// exists.
func GcpRuntimeServiceAccountFor(deploymentName, project string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", deploymentName, project)
}

// GcpDatabaseUserFor is the database-side username for a service account under
// IAM auth. Cloud SQL Postgres and AlloyDB register a service account WITHOUT
// its .gserviceaccount.com suffix; Cloud SQL MySQL usernames are the part
// before the @ (verified live against both engines).
func GcpDatabaseUserFor(saEmail, engine string) string {
	if engine == "mysql" {
		local, _, _ := strings.Cut(saEmail, "@")
		return local
	}
	return strings.TrimSuffix(saEmail, ".gserviceaccount.com")
}

// GcpStackInput carries everything GcpDeployInputs needs to render the
// template's inputs.
type GcpStackInput struct {
	AgentID   string
	TenantID  string
	Image     string
	Endpoints Endpoints
	Targets   []GcpTarget
	// Network is the VPC self-link the collector instance joins — discovered
	// from the first database, or forced by flag.
	Network string
	Region  string
	// DeploymentName feeds the runtime service account naming contract.
	DeploymentName string
	Project        string
	ServerSecret   string
	// DBPassword may be empty when every target uses IAM auth. Like the aws
	// target, the value rides its own input (into Secret Manager), never the
	// config document.
	DBPassword      string
	CommandsEnabled bool
}

// GcpDeployInputs renders the template's input variables. The config is the
// same base64 TOML carrier the aws target uses — inspectable on the
// deployment, round-trippable by the CLI — and secrets ride separate inputs.
func GcpDeployInputs(in GcpStackInput) (map[string]string, error) {
	configTOML, err := GcpConfigTOML(in.AgentID, in.TenantID, in.Targets, in.Endpoints, in.CommandsEnabled)
	if err != nil {
		return nil, err
	}
	encoded, err := EncodeConfig(configTOML)
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
	}, nil
}
