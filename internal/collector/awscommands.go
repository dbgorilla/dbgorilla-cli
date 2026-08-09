package collector

import "strings"

// Query-analysis commands the collector may run against a monitored database.
// Enabling them lets the collector issue read-only analysis queries; a policy
// that forbids the collector issuing any queries leaves them off via the global
// gate (DBG__COMMANDS__ENABLED). Which commands each component may run is
// per-component (DBG__COMPONENT__i__COMMANDS__k), and clamped to what the
// component's engine supports.
const (
	CmdExecuteQuery = "execute_query" // read-only queries against system / pg_stat views
	CmdExplain      = "explain"       // EXPLAIN / EXPLAIN ANALYZE plans
)

// componentEngine is the collector engine for every AWS target — RDS and Aurora
// are both Postgres to the collector. Commands are clamped to what it supports.
const componentEngine = "postgres"

// commandCatalog is the ordered set of commands each engine supports; it is the
// single source of truth for engine clamping and the interactive picker.
var commandCatalog = map[string][]string{
	"postgres": {CmdExecuteQuery, CmdExplain},
}

// AwsCommandCatalog lists every command an AWS collector component can run, in a
// stable order — the options a picker offers.
func AwsCommandCatalog() []string {
	return append([]string(nil), commandCatalog[componentEngine]...)
}

// AwsCommandsFor clamps requested commands to those an AWS component supports,
// preserving catalog order and dropping unknowns/duplicates. An empty request
// means "all supported" — the sensible default when analysis is enabled.
func AwsCommandsFor(requested []string) []string {
	return clampCommands(componentEngine, requested)
}

func clampCommands(engine string, requested []string) []string {
	valid := commandCatalog[engine]
	if len(requested) == 0 {
		return append([]string(nil), valid...)
	}
	want := map[string]bool{}
	for _, r := range requested {
		want[strings.TrimSpace(r)] = true
	}
	var out []string
	for _, c := range valid { // catalog order, no duplicates
		if want[c] {
			out = append(out, c)
		}
	}
	return out
}
