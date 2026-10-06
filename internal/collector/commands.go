package collector

import "strings"

// Query-analysis commands the collector may run against a monitored database.
// explain and collect_statistics are on by default (DefaultCommands);
// execute_query is off unless the operator turns it on. Which commands each
// component may run is per-component, and clamped to what the component's
// engine supports. The collector enforces the bounds, not this CLI:
// execute_query runs in a read-only transaction that is always rolled back,
// with a 30-second statement timeout and at most 1,000 rows; explain is
// plan-only (no ANALYZE), so the query is never executed; collect_statistics
// copies optimizer statistics and no table rows.
const (
	CmdExecuteQuery      = "execute_query"      // read-only checks, e.g. pg_stat_* and system views
	CmdExplain           = "explain"            // EXPLAIN without ANALYZE: the plan, never the run
	CmdCollectStatistics = "collect_statistics" // table statistics only, so a sandbox plans like production
)

// Fork commands the collector may run when the operator enables fast forks
// (--fast-fork). These are provider commands (Instaclustr), not engine
// commands, but the CLI writes an explicit per-component commands list and
// the collector clamps to engine ∪ provider — so omitting them silently
// drops fork capability.
var ForkCommands = []string{
	"fork_preflight",
	"fork_snapshot_trigger",
	"fork_create",
	"fork_status",
	"fork_list",
	"fork_allowlist",
	"fork_connection_info",
	"fork_delete",
	"fork_execute_query",
	"fork_execute_statement",
}

// componentEngine is the collector engine for every AWS target — RDS and Aurora
// are both Postgres to the collector.
const componentEngine = "postgres"

// commandCatalog is the ordered set of commands each engine supports; it is the
// single source of truth for engine clamping and the interactive picker. It
// mirrors the collector's engines, which support the same three on both.
var commandCatalog = map[string][]string{
	"postgres": {CmdExecuteQuery, CmdExplain, CmdCollectStatistics},
	"mysql":    {CmdExecuteQuery, CmdExplain, CmdCollectStatistics},
}

// defaultCommands is what a new database gets when nobody chose: explain and
// collect_statistics. Neither runs the operator's queries or reads a table
// row. explain returns the plan. collect_statistics copies the optimizer's
// statistics, which is what lets a recommendation be tested on a replay of
// the production planner before it is made; without it the test cannot run
// and the recommendation goes out unchecked. execute_query reads data and
// stays opt-in.
var defaultCommands = []string{CmdExplain, CmdCollectStatistics}

// minCollectorVersion is the first collector release that understands each
// command. A collector reads its config with a closed command list, so a name
// it does not know is a parse error and the whole config is refused, not just
// the one command. explain and execute_query predate every image this CLI can
// install, so only collect_statistics is listed.
var minCollectorVersion = map[string]string{
	CmdCollectStatistics: "0.5.0",
}

// CommandsBeyondImage lists the commands the collector image cannot be
// expected to understand, so the caller can warn before the collector refuses
// its config. A tag that is not a version (latest, a digest, a custom build)
// gives no answer and reports nothing: refusing on an unknown tag would block
// every custom image.
func CommandsBeyondImage(image string, commands []string) []string {
	have, ok := parseVersion(ImageTagOf(image))
	if !ok {
		return nil
	}
	var out []string
	for _, c := range commands {
		min, listed := minCollectorVersion[c]
		if !listed {
			continue
		}
		need, _ := parseVersion(min)
		if compareVersions(need, have) < 0 { // the image is older than the first release with c
			out = append(out, c)
		}
	}
	return out
}

// DefaultCommands is the commands a new database of this engine gets when no
// flag or checklist answer says otherwise.
func DefaultCommands(engine string) []string {
	return CommandsFor(engine, defaultCommands)
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

func (c *Component) CommandEngine() string        { return c.Engine }
func (c *Component) CommandList() []string        { return c.Commands }
func (c *Component) SetCommandList(cmds []string) { c.Commands = cmds }

// CommandRequest is how the caller's flags landed, decoupled from cobra: the
// command layer reads the flags, this layer applies the precedence.
type CommandRequest struct {
	// ForcedOff is an explicit hard "no query analysis" — --enable-commands=false
	// or --commands="" — for policies that forbid the collector issuing any
	// queries, explain included. It clears every database, --config lists
	// included, and skips the prompt.
	ForcedOff bool
	// Enabled is --enable-commands=true: every command the engine supports, for
	// each database that has no list of its own and no --commands. It skips the
	// prompt.
	Enabled bool
	// Explicit reports that --commands was given (even empty), which applies to
	// every database and suppresses the interactive checklist.
	Explicit bool
	// Commands is the --commands value, unclamped.
	Commands []string
}

// ResolveCommands settles, per database, which query-analysis commands the
// collector may run, storing them on each target, and reports whether analysis
// is on at all. That gate is implicit: on iff at least one database ended up
// with a command.
//
// Precedence per component: commands from --config win; else an explicit
// --commands applies to all; else --enable-commands grants everything the engine
// supports; else prompt, if the caller supplied one; else DefaultCommands. All
// are engine-clamped.
//
// prompt is the interactive per-database checklist. nil means non-interactive,
// which gives the database DefaultCommands — keeping the terminal handling in
// the command layer and this precedence testable on its own.
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
			// An empty CommandsFor request means "all", so a --commands value
			// that names nothing (",") must not reach it.
			if len(req.Commands) == 0 {
				t.SetCommandList(nil)
				break
			}
			t.SetCommandList(CommandsFor(engine, req.Commands))
		case req.Enabled:
			t.SetCommandList(CommandsFor(engine, nil))
		case prompt != nil:
			t.SetCommandList(prompt(targets[i]))
		default:
			t.SetCommandList(DefaultCommands(engine))
		}
		if len(t.CommandList()) > 0 {
			enabled = true // implicit gate: any database with a command turns it on
		}
	}
	return enabled
}
