package collector

import (
	"fmt"
	"strings"
)

// Shared by the cloud deploy targets (aws, gcp): the parts of an install that do
// not care which cloud is underneath. Each target adds its own discovery,
// deployment API and template; the pieces here are the common spine, kept in
// one place so a third target extends them rather than copying them.

// baseConfig is the [dbgorilla] identity block plus the global sections every
// rendered collector.toml carries. Callers append their [[component]] blocks.
// Secrets are never rendered: the config references them as ${VAR} and the
// runtime injects the real values.
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

// TargetChoice is one candidate database auto-selection found: its id and the
// provider type it belongs to (aws_rds, aws_aurora, cloud_sql, alloydb).
type TargetChoice struct {
	ID           string
	ProviderType string
}

// ProviderLabel is the human-facing name of a provider type, for pickers and
// error messages. An empty type is the aws target's default, RDS.
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
// monitorable database. It carries the candidates so an interactive caller can
// offer a picker; a non-interactive caller just surfaces Error() and the user
// re-runs with --db-instance-id. A distinct type so the command layer can
// errors.As it without string-matching.
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

// selectSolo picks the single candidate. More than one is an
// *AmbiguousTargetError, so an interactive caller can prompt; none returns the
// caller's own actionable error, which names what was searched and the flag
// that bypasses the search. Selection never guesses. Pure, for testability.
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

// logCursor dedupes a polled, timestamp-ordered log stream. Every poll restarts
// at the newest timestamp already printed — inclusive, because advancing past
// it (ts+1) would drop entries written in that same instant that had not been
// read yet — so the entries at exactly that timestamp come back each time and
// need suppressing. Only their ids are remembered, which keeps memory bounded
// no matter how long a --follow runs.
type logCursor struct {
	newest int64
	seen   map[string]bool
}

func newLogCursor(start int64) *logCursor {
	return &logCursor{newest: start, seen: map[string]bool{}}
}

// accept reports whether an entry has not been printed yet, and records it.
// An entry newer than the cursor advances it and forgets the older ids.
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
