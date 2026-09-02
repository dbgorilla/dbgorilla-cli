package collector

import "strings"

// Query-analysis commands the collector may run against a monitored database.
// Enabling them lets the collector issue read-only analysis queries; a policy
// that forbids the collector issuing any queries leaves them off via the global
// gate ([commands] enabled). Which commands each component may run is
// per-component, and clamped to what the component's engine supports.
const (
	CmdExecuteQuery = "execute_query" // read-only queries against system / pg_stat views
	CmdExplain      = "explain"       // EXPLAIN / EXPLAIN ANALYZE plans
)

// componentEngine is the collector engine for every AWS target — RDS and Aurora
// are both Postgres to the collector.
const componentEngine = "postgres"

// commandCatalog is the ordered set of commands each engine supports; it is the
// single source of truth for engine clamping and the interactive picker.
//
// MySQL (a Cloud SQL engine) has no entry: the collector's MySQL query-analysis
// support is not modelled here yet, so a MySQL component gets no commands and,
// on its own, leaves analysis off.
var commandCatalog = map[string][]string{
	"postgres": {CmdExecuteQuery, CmdExplain},
}

// CommandCatalog lists every command a component of the given engine can run,
// in a stable order — the options a picker offers. Unknown engines have none.
func CommandCatalog(engine string) []string {
	return append([]string(nil), commandCatalog[engine]...)
}

// CommandsFor clamps requested commands to those the engine supports,
// preserving catalog order and dropping unknowns/duplicates. An empty request
// means "all supported" — the sensible default when analysis is enabled.
func CommandsFor(engine string, requested []string) []string {
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

// CommandTarget is what ResolveCommands needs from a monitored database: the
// engine its commands are clamped to, and where the chosen list lives. Each
// cloud target implements it on its pointer type.
type CommandTarget interface {
	CommandEngine() string
	CommandList() []string
	SetCommandList([]string)
}

func (t *AwsTarget) CommandEngine() string     { return componentEngine }
func (t *AwsTarget) CommandList() []string     { return t.Commands }
func (t *AwsTarget) SetCommandList(c []string) { t.Commands = c }

func (t *GcpTarget) CommandEngine() string     { return t.Engine }
func (t *GcpTarget) CommandList() []string     { return t.Commands }
func (t *GcpTarget) SetCommandList(c []string) { t.Commands = c }

// CommandRequest is how the caller's flags landed, decoupled from cobra: the
// command layer reads the flags, this layer applies the precedence.
type CommandRequest struct {
	// ForcedOff is an explicit hard "no query analysis" — --enable-commands=false
	// or --commands="" — for policies that forbid the collector issuing any
	// queries. It clears every database and skips the prompt.
	ForcedOff bool
	// Explicit reports that --commands was given (even empty), which applies to
	// every database and suppresses the interactive checklist.
	Explicit bool
	// Commands is the --commands value, unclamped.
	Commands []string
}

// ResolveCommands settles, per database, which query-analysis commands the
// collector may run, storing them on each target, and reports whether analysis
// is on at all. That gate is implicit: on iff at least one database ended up
// with a command. There is no separate yes/no switch — the per-database choice
// is the switch.
//
// Precedence per component: commands from --config win; else an explicit
// --commands applies to all; else prompt, if the caller supplied one; else every
// command the engine supports. All are engine-clamped.
//
// prompt is the interactive per-database checklist. nil means non-interactive,
// which takes the full-catalog default — keeping the terminal handling in the
// command layer and this precedence testable on its own.
func ResolveCommands[T any, PT interface {
	*T
	CommandTarget
}](targets []T, req CommandRequest, prompt func(T) []string) bool {
	if req.ForcedOff {
		for i := range targets {
			PT(&targets[i]).SetCommandList(nil)
		}
		return false
	}
	enabled := false
	for i := range targets {
		t := PT(&targets[i])
		engine := t.CommandEngine()
		switch {
		case len(t.CommandList()) > 0: // from --config: keep, clamped
			t.SetCommandList(CommandsFor(engine, t.CommandList()))
		case req.Explicit:
			t.SetCommandList(CommandsFor(engine, req.Commands))
		case prompt != nil:
			t.SetCommandList(prompt(targets[i]))
		default:
			t.SetCommandList(CommandsFor(engine, nil))
		}
		if len(t.CommandList()) > 0 {
			enabled = true // implicit gate: any database with a command turns it on
		}
	}
	return enabled
}
