package collector

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// AwsConfig is the multi-database install file, passed as `dbg collector install
// --target aws --config FILE`. Each [[database]] entry becomes one collector
// component (a DBG__COMPONENT__i__* env block) plus one rds-db:connect grant on
// the shared task role. A single-database install needs no file — the --db-*
// flags describe the one database — so this exists only for the N>1 case.
//
// Networking (subnets, security group, public-IP) is deliberately NOT per
// database: one Fargate task runs all N components, so it has a single VPC
// network config. Every listed database must therefore be reachable from that
// one network — practically, they share a VPC. Networking comes from the
// top-level --subnets/--security-group-id flags, or is discovered from the first
// database.
type AwsConfig struct {
	Databases []AwsDatabase `toml:"database"`
}

// AwsDatabase is one [[database]] entry. Only instance-id is required; anything
// left unset is auto-discovered from RDS/Aurora (same rules as the single-DB
// flags). Field names mirror the --db-* flags so the file reads like the flags.
type AwsDatabase struct {
	InstanceID    string   `toml:"instance-id"`     // RDS instance id or Aurora cluster id (required)
	Name          string   `toml:"name"`            // display name in DBGorilla (default: the id)
	ProviderType  string   `toml:"provider-type"`   // aws_rds | aws_aurora (auto-detected if empty)
	User          string   `toml:"user"`            // IAM database user (default: dbgorilla)
	Databases     []string `toml:"databases"`       // database names to monitor (empty = all)
	SSLMode       string   `toml:"ssl-mode"`        // libpq ssl_mode (default: verify-full)
	DbiResourceID string   `toml:"dbi-resource-id"` // scopes rds-db:connect (discovered)
	Host          string   `toml:"host"`            // endpoint address (discovered)
	Port          int      `toml:"port"`            // endpoint port (discovered; default 5432)
	Commands      []string `toml:"commands"`        // query-analysis commands to allow (default: all, when enabled)
}

// LoadAwsConfig reads and validates a multi-database install file.
func LoadAwsConfig(path string) (*AwsConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading --config %q: %w", path, err)
	}
	var c AwsConfig
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing --config %q: %w", path, err)
	}
	if len(c.Databases) == 0 {
		return nil, fmt.Errorf("--config %q has no [[database]] entries", path)
	}
	for i, d := range c.Databases {
		if d.InstanceID == "" {
			return nil, fmt.Errorf("--config %q: [[database]] #%d is missing instance-id", path, i+1)
		}
	}
	return &c, nil
}

// Seed converts a config entry into an AwsTarget with only the explicitly-set
// fields populated; DiscoverAwsTarget fills the rest (explicit wins).
func (d AwsDatabase) Seed() AwsTarget {
	return AwsTarget{
		Name:          d.Name,
		InstanceID:    d.InstanceID,
		DbiResourceID: d.DbiResourceID,
		Host:          d.Host,
		Port:          d.Port,
		User:          d.User,
		Databases:     d.Databases,
		SSLMode:       d.SSLMode,
		ProviderType:  d.ProviderType,
		Commands:      d.Commands,
	}
}
