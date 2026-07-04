// Package preflight runs read-only checks against a PostgreSQL instance to
// answer: "is this database ready to be a DBGorilla collector source?"
//
// Each check returns a storage-agnostic Result with a severity and, when
// relevant, copy-pastable fix commands. The check logic is isolated from pgx
// via the Inspector port; the pgxInspector adapter is the only thing that
// imports pgx, so the rules are unit-testable against an in-memory fake.
package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Severity classifies a preflight outcome.
type Severity string

const (
	OK   Severity = "ok"
	Warn Severity = "warn"
	Fail Severity = "fail"
)

// Result is the outcome of one preflight check.
type Result struct {
	Name     string
	Severity Severity
	Detail   string
	// Fix, when non-empty, is copy-pastable remediation. Empty on OK.
	Fix []string
}

// Report aggregates all check results from a run.
type Report struct {
	Results []Result
}

// Failed returns true if any result is a Fail.
func (r Report) Failed() bool {
	for _, x := range r.Results {
		if x.Severity == Fail {
			return true
		}
	}
	return false
}

// HasWarnings returns true if any result is a Warn.
func (r Report) HasWarnings() bool {
	for _, x := range r.Results {
		if x.Severity == Warn {
			return true
		}
	}
	return false
}

// Inspector is the port the checks read DB state through.
type Inspector interface {
	Summary() string
	ServerVersionNum(ctx context.Context) (string, error)
	ShowParam(ctx context.Context, name string) (string, error)
	HasExtension(ctx context.Context, name string) (bool, error)
	CanReadStats(ctx context.Context) error
}

// Run opens a pgx connection to dsn and runs all checks. A connection failure
// short-circuits to a single Fail result.
func Run(ctx context.Context, dsn string) Report {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return Report{Results: []Result{{
			Name:     "connect",
			Severity: Fail,
			Detail:   fmt.Sprintf("cannot connect: %v", err),
			Fix: []string{
				"Verify the host/port and that PostgreSQL is reachable.",
				"Check the read-only user and password.",
				"If the server requires TLS, set --ssl-mode require (or verify-full).",
			},
		}}}
	}
	defer func() { _ = conn.Close(ctx) }()
	return RunWith(ctx, &pgxInspector{conn: conn, dsn: dsn})
}

// RunWith runs the checks against a given Inspector (for tests / reused conns).
func RunWith(ctx context.Context, ins Inspector) Report {
	results := []Result{{Name: "connect", Severity: OK, Detail: ins.Summary()}}
	results = append(results,
		CheckServerVersion(ctx, ins),
		CheckSharedPreload(ctx, ins),
		CheckExtension(ctx, ins),
		CheckStatsGrants(ctx, ins),
		CheckTrackIOTiming(ctx, ins),
		CheckTrackFunctions(ctx, ins),
	)
	return Report{Results: results}
}

// CheckServerVersion requires Postgres 13+.
func CheckServerVersion(ctx context.Context, ins Inspector) Result {
	versionStr, err := ins.ServerVersionNum(ctx)
	if err != nil {
		return Result{Name: "server version", Severity: Fail, Detail: err.Error()}
	}
	versionNum, err := strconv.Atoi(strings.TrimSpace(versionStr))
	if err != nil {
		return Result{Name: "server version", Severity: Fail, Detail: fmt.Sprintf("unexpected version format %q", versionStr)}
	}
	major := versionNum / 10000
	if major < 13 {
		return Result{
			Name:     "server version",
			Severity: Fail,
			Detail:   fmt.Sprintf("Postgres %d -- dbg requires 13 or newer", major),
			Fix:      []string{"Upgrade to Postgres 13+ to use the collector."},
		}
	}
	return Result{Name: "server version", Severity: OK, Detail: fmt.Sprintf("Postgres %d", major)}
}

// CheckSharedPreload requires pg_stat_statements in shared_preload_libraries.
func CheckSharedPreload(ctx context.Context, ins Inspector) Result {
	libs, err := ins.ShowParam(ctx, "shared_preload_libraries")
	if err != nil {
		return Result{Name: "shared_preload_libraries", Severity: Fail, Detail: err.Error()}
	}
	if !ContainsLib(libs, "pg_stat_statements") {
		return Result{
			Name:     "shared_preload_libraries",
			Severity: Fail,
			Detail:   fmt.Sprintf("pg_stat_statements not loaded (current: %q)", libs),
			Fix: []string{
				"As a superuser, add pg_stat_statements to shared_preload_libraries:",
				"  ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';",
				"Then RESTART the server (a reload is not enough for this parameter).",
			},
		}
	}
	return Result{Name: "shared_preload_libraries", Severity: OK, Detail: libs}
}

// CheckExtension requires the pg_stat_statements extension in the current DB.
func CheckExtension(ctx context.Context, ins Inspector) Result {
	present, err := ins.HasExtension(ctx, "pg_stat_statements")
	if err != nil {
		return Result{Name: "pg_stat_statements extension", Severity: Fail, Detail: err.Error()}
	}
	if !present {
		return Result{
			Name:     "pg_stat_statements extension",
			Severity: Fail,
			Detail:   "extension not created in this database",
			Fix:      []string{"CREATE EXTENSION pg_stat_statements;"},
		}
	}
	return Result{Name: "pg_stat_statements extension", Severity: OK, Detail: "installed"}
}

// CheckStatsGrants requires the role to be able to read pg_stat_statements.
func CheckStatsGrants(ctx context.Context, ins Inspector) Result {
	err := ins.CanReadStats(ctx)
	if err == nil {
		return Result{Name: "stats read permission", Severity: OK, Detail: "pg_read_all_stats (or equivalent) granted"}
	}
	msg := err.Error()
	if strings.Contains(msg, "permission") || strings.Contains(msg, "denied") {
		return Result{
			Name:     "stats read permission",
			Severity: Fail,
			Detail:   "current role cannot read pg_stat_statements",
			Fix: []string{
				"As a superuser, grant read access on server stats:",
				"  GRANT pg_read_all_stats TO <your_role>;",
				"Reconnect with the updated role.",
			},
		}
	}
	return Result{Name: "stats read permission", Severity: Warn, Detail: msg}
}

// CheckTrackIOTiming warns if track_io_timing is off.
func CheckTrackIOTiming(ctx context.Context, ins Inspector) Result {
	v, err := ins.ShowParam(ctx, "track_io_timing")
	if err != nil {
		return Result{Name: "track_io_timing", Severity: Warn, Detail: err.Error()}
	}
	if v != "on" {
		return Result{
			Name:     "track_io_timing",
			Severity: Warn,
			Detail:   "track_io_timing = off -- block I/O timing will be unavailable",
			Fix: []string{
				"For richer query I/O stats, enable timing:",
				"  ALTER SYSTEM SET track_io_timing = 'on';",
				"  SELECT pg_reload_conf();",
			},
		}
	}
	return Result{Name: "track_io_timing", Severity: OK, Detail: "on"}
}

// CheckTrackFunctions warns if track_functions is 'none'.
func CheckTrackFunctions(ctx context.Context, ins Inspector) Result {
	v, err := ins.ShowParam(ctx, "track_functions")
	if err != nil {
		return Result{Name: "track_functions", Severity: Warn, Detail: err.Error()}
	}
	if v == "none" {
		return Result{
			Name:     "track_functions",
			Severity: Warn,
			Detail:   "track_functions = none -- pg_stat_user_functions will be empty",
			Fix: []string{
				"If you use PL/pgSQL (or other procedural) functions, enable tracking:",
				"  ALTER SYSTEM SET track_functions = 'pl';",
				"  SELECT pg_reload_conf();",
			},
		}
	}
	return Result{Name: "track_functions", Severity: OK, Detail: v}
}

// --- helpers --------------------------------------------------------------

// ContainsLib reports whether a comma-separated preload list contains needle.
func ContainsLib(libs, needle string) bool {
	for _, p := range strings.Split(libs, ",") {
		if strings.TrimSpace(p) == needle {
			return true
		}
	}
	return false
}

// DSNSummary returns a host/db-only view of a DSN, hiding credentials.
func DSNSummary(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return "connected to " + rest
	}
	return "connected"
}

// --- pgx adapter ----------------------------------------------------------

type pgxInspector struct {
	conn *pgx.Conn
	dsn  string
}

func (p *pgxInspector) Summary() string { return DSNSummary(p.dsn) }

func (p *pgxInspector) ServerVersionNum(ctx context.Context) (string, error) {
	var v string
	err := p.conn.QueryRow(ctx, "SHOW server_version_num").Scan(&v)
	return v, err
}

func (p *pgxInspector) ShowParam(ctx context.Context, name string) (string, error) {
	var v string
	err := p.conn.QueryRow(ctx, "SHOW "+quoteIdent(name)).Scan(&v)
	return v, err
}

func (p *pgxInspector) HasExtension(ctx context.Context, name string) (bool, error) {
	var present bool
	err := p.conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", name,
	).Scan(&present)
	return present, err
}

func (p *pgxInspector) CanReadStats(ctx context.Context) error {
	var count int
	return p.conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_statements LIMIT 1").Scan(&count)
}

// quoteIdent wraps an identifier in double quotes. The package only passes a
// fixed set of literal parameter names; this is defense-in-depth.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
