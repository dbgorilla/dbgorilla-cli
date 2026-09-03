package collector

import (
	"errors"
	"fmt"
	"strings"
)

// Shared by the cloud deploy targets (aws, gcp).

// ErrDeployBusy marks a deploy refused because the runtime is already
// converging under another operation. Callers must not roll back on it.
var ErrDeployBusy = errors.New("deployment busy")

// ErrDeployUnknown marks a deploy whose outcome could not be observed: the
// client lost the operation, not the server. Callers must leave the runtime,
// the identity and local state in place.
var ErrDeployUnknown = errors.New("deploy outcome unknown")

// baseConfig is the [dbgorilla] identity block plus the global sections every
// rendered collector.toml carries. Secrets are ${VAR} references only.
func baseConfig(agentID, tenantID string, eps Endpoints, commandsEnabled bool) Config {
	return Config{
		Dbgorilla: Dbgorilla{
			AgentID:      agentID,
			TenantID:     tenantID,
			Secret:       "${" + SecretEnv + "}",
			OpampBaseURL: eps.OpampBaseURL,
			OtlpBaseURL:  eps.OtlpBaseURL,
			AuthBaseURL:  eps.AuthBaseURL,
		},
		Topology: Topology{Interval: "60s"},
		Commands: Commands{Enabled: commandsEnabled},
	}
}

// TargetChoice is one candidate database auto-selection found.
type TargetChoice struct {
	ID           string
	ProviderType string
}

// ProviderLabel is the human-facing name of a provider type. An empty type is
// the aws target's default, RDS.
func ProviderLabel(providerType string) string {
	switch providerType {
	case "aws_aurora":
		return "Aurora"
	case "cloud_sql":
		return "Cloud SQL"
	case "alloydb":
		return "AlloyDB"
	default:
		return "RDS"
	}
}

// AmbiguousTargetError is returned when auto-selection finds more than one
// monitorable database; it carries the candidates for a picker.
type AmbiguousTargetError struct {
	Choices []TargetChoice
}

func (e *AmbiguousTargetError) Error() string {
	names := make([]string, 0, len(e.Choices))
	for _, c := range e.Choices {
		names = append(names, fmt.Sprintf("%s (%s)", c.ID, ProviderLabel(c.ProviderType)))
	}
	return fmt.Sprintf("found multiple databases: %s; pass --db-instance-id to choose one",
		strings.Join(names, ", "))
}

// Candidates returns the choices in the stable order the listing produced.
func (e *AmbiguousTargetError) Candidates() []TargetChoice { return e.Choices }

// selectSolo picks the single candidate: none returns the caller's error, more
// than one an *AmbiguousTargetError. It never guesses.
func selectSolo(choices []TargetChoice, none error) (TargetChoice, error) {
	switch len(choices) {
	case 0:
		return TargetChoice{}, none
	case 1:
		return choices[0], nil
	default:
		return TargetChoice{}, &AmbiguousTargetError{Choices: choices}
	}
}

// logCursor dedupes a polled, timestamp-ordered log stream. Each poll restarts
// at the newest timestamp already printed (inclusive, so same-instant entries
// written after the last read are not lost); only that instant's ids are kept.
type logCursor struct {
	newest int64
	seen   map[string]bool
}

func newLogCursor(start int64) *logCursor {
	return &logCursor{newest: start, seen: map[string]bool{}}
}

// accept reports whether an entry has not been printed yet, and records it.
func (c *logCursor) accept(id string, ts int64) bool {
	if id != "" && c.seen[id] {
		return false
	}
	if ts > c.newest {
		c.newest = ts
		c.seen = map[string]bool{}
	}
	if id != "" && ts == c.newest {
		c.seen[id] = true
	}
	return true
}
