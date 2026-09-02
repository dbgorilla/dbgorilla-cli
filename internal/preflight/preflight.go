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
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CloudNativePG (CNPG) runs PostgreSQL as Kubernetes pods and owns
// postgresql.conf declaratively, so the psql-shaped remediations below do not
// apply to it. Both notes are emitted unconditionally: this package holds a DSN
// and no provider, so it cannot tell a CNPG server from any other self-hosted
// one. The cost is one extra line for a non-CNPG reader; the alternative is
// telling a CNPG operator to run a command their cluster refuses.
//
// ALTER SYSTEM has been disabled by default since CNPG 1.22 and fails with
// `could not open file "postgresql.auto.conf": Permission denied`. Declaring any
// `pg_stat_statements.*` parameter makes the operator add the library to
// shared_preload_libraries and run CREATE EXTENSION in every database, so the
// two psql steps below collapse into one manifest key.
const (
	cnpgParametersNote = "On CloudNativePG, ALTER SYSTEM is disabled by default -- set this " +
		"under spec.postgresql.parameters in the Cluster resource instead."
	cnpgStatStatementsNote = "On CloudNativePG, set a spec.postgresql.parameters key beginning " +
		"`pg_stat_statements.` (e.g. pg_stat_statements.max) -- the operator then loads the " +
		"library and creates the extension in every database for you."
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
	// MaintenanceDBHasExtension checks for the extension in the `postgres`
	// maintenance database -- the DB the collector's capability probe connects
	// to, which can differ from the target DB this preflight is connected to.
	MaintenanceDBHasExtension(ctx context.Context, name string) (bool, error)
	CanReadStats(ctx context.Context) error
	UnreadableUserTables(ctx context.Context) (int, error)
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
			Fix:      tlsAwareConnectFix(err),
		}}}
	}
	defer func() { _ = conn.Close(ctx) }()
	return RunWith(ctx, &pgxInspector{conn: conn, dsn: dsn})
}

// TLSSupport is what a probe learned about a server's TLS capability.
type TLSSupport int

const (
	// TLSUnknown means the probe could not tell -- the server was unreachable,
	// or it failed for a reason unrelated to TLS. Never act on this; let the
	// real preflight report the actual error.
	TLSUnknown TLSSupport = iota
	// TLSSupported means the server completed a TLS handshake.
	TLSSupported
	// TLSUnsupported means the server explicitly refused TLS.
	TLSUnsupported
)

// ProbeTLS asks the server whether it speaks TLS at all, so the CLI can react
// to what the server actually is instead of making the user guess a flag.
//
// The probe deliberately uses sslmode=require, not verify-full: require tests
// only whether a TLS handshake is possible, so a server with TLS behind a
// private CA still answers "supported" (its certificate is a separate problem
// with a separate fix). An authentication failure also means supported -- the
// handshake got far enough for the server to reject the credentials.
//
// dsn must already carry sslmode=require; ProbeTLSDSN builds that form.
func ProbeTLS(ctx context.Context, dsn string) TLSSupport {
	conn, err := pgx.Connect(ctx, dsn)
	if err == nil {
		_ = conn.Close(ctx)
		return TLSSupported
	}
	if IsTLSUnsupported(err) {
		return TLSUnsupported
	}
	if isAuthFailure(err) {
		return TLSSupported
	}
	return TLSUnknown
}

// ProbeTLSDSN rewrites a DSN's sslmode to "require" for the capability probe.
func ProbeTLSDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set("sslmode", "require")
	u.RawQuery = q.Encode()
	return u.String()
}

// IsTLSUnsupported reports whether an error is the server saying it has no TLS.
func IsTLSUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "server does not support ssl") ||
		strings.Contains(msg, "server refused tls")
}

// isAuthFailure reports whether the server answered but rejected the login,
// which still proves the transport worked.
func isAuthFailure(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "password authentication failed") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "role") && strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no pg_hba.conf entry")
}

// tlsAwareConnectFix builds the remediation for a failed connection. The
// direction matters: a server that does not SUPPORT TLS and a server that
// REQUIRES it fail with opposite fixes, and telling a local-dev user to add
// more TLS when their database has none sends them the wrong way.
func tlsAwareConnectFix(err error) []string {
	base := []string{
		"Verify the host/port and that PostgreSQL is reachable.",
		"Check the read-only user and password.",
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "server does not support ssl"):
		// Typical local dev: stock Postgres ships with ssl=off.
		return append(base,
			"This server has TLS disabled, so set --ssl-mode disable.",
			"(Local Postgres — Docker image, Postgres.app, Homebrew — has no TLS by default.)",
		)
	case strings.Contains(msg, "server refused tls") ||
		strings.Contains(msg, "sslmode") ||
		strings.Contains(msg, "tls error"):
		return append(base,
			"TLS negotiation failed. If the server has no TLS, set --ssl-mode disable;",
			"if it has TLS with a private CA, keep verify-full and pass --ca-cert /path/to/ca.pem.",
		)
	default:
		return append(base,
			"Set --ssl-mode to match the server: disable (no TLS), require, verify-ca, or verify-full.",
		)
	}
}

// CheckWorkload opens a connection to dsn and returns ONLY the workload
// (pg_stat_statements) capability check -- the `postgres` maintenance-DB probe
// that determines whether query/workload data flows. It lets `dbg collector
// status` answer "topology works but the Queries views are empty" without
// running the full preflight. A connection failure degrades to a Warn so status
// never hard-errors on a transiently-unreachable DB.
func CheckWorkload(ctx context.Context, dsn string) Result {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return Result{
			Name:     "workload",
			Severity: Warn,
			Detail:   fmt.Sprintf("could not connect to the database to check the workload capability: %v", err),
		}
	}
	defer func() { _ = conn.Close(ctx) }()
	res := CheckExtension(ctx, &pgxInspector{conn: conn, dsn: dsn})
	res.Name = "workload"
	return res
}

// RunWith runs the checks against a given Inspector (for tests / reused conns).
func RunWith(ctx context.Context, ins Inspector) Report {
	results := []Result{{Name: "connect", Severity: OK, Detail: ins.Summary()}}
	results = append(results,
		CheckServerVersion(ctx, ins),
		CheckSharedPreload(ctx, ins),
		CheckExtension(ctx, ins),
		CheckStatsGrants(ctx, ins),
		CheckTopologyGrants(ctx, ins),
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
		// Warn, not Fail: the collector runs and reports schema topology without
		// pg_stat_statements. It gates query-performance data only. Blocking here
		// made a stock local Postgres un-onboardable, forcing an ALTER SYSTEM and
		// a full database restart before the user had seen anything work.
		return Result{
			Name:     "shared_preload_libraries",
			Severity: Warn,
			Detail:   fmt.Sprintf("pg_stat_statements not loaded (current: %q) -- query-performance data will be unavailable; everything else works", libs),
			Fix: []string{
				"To capture query performance, add pg_stat_statements as a superuser:",
				"  ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';",
				"Then RESTART the server (a reload is not enough for this parameter).",
				cnpgStatStatementsNote,
			},
		}
	}
	return Result{Name: "shared_preload_libraries", Severity: OK, Detail: libs}
}

// CheckExtension requires pg_stat_statements in the `postgres` MAINTENANCE
// database -- the DB the collector's capability probe actually connects to.
// This is deliberately NOT (only) the target/business DB: if the extension
// lives only in the target DB, the collector probes `postgres`, finds nothing,
// and silently advertises NO workload -- topology flows but the Queries views
// stay permanently empty with no error. So we gate on the maintenance DB and
// call out the target-only divergence explicitly (dbgorilla-cli#14).
func CheckExtension(ctx context.Context, ins Inspector) Result {
	inMaint, err := ins.MaintenanceDBHasExtension(ctx, "pg_stat_statements")
	if err != nil {
		// Could not reach/inspect the postgres maintenance DB. Warn rather than
		// hard-fail -- we cannot confirm the workload capability either way.
		return Result{
			Name:     "pg_stat_statements extension",
			Severity: Warn,
			Detail:   fmt.Sprintf("could not verify pg_stat_statements in the postgres maintenance DB (the collector probes it there for workload): %v", err),
		}
	}
	if inMaint {
		return Result{Name: "pg_stat_statements extension", Severity: OK, Detail: "installed in the postgres maintenance DB"}
	}
	// Missing in `postgres`. If it is (misleadingly) present in the target DB,
	// this is the silent-empty-workload trap: setup/preflight look fine but the
	// collector probes `postgres` and finds nothing.
	if inTarget, terr := ins.HasExtension(ctx, "pg_stat_statements"); terr == nil && inTarget {
		return Result{
			Name:     "pg_stat_statements extension",
			Severity: Fail,
			Detail:   "pg_stat_statements exists in the target database but NOT in the `postgres` maintenance database -- the collector probes `postgres` for the workload capability, so query/workload data will be silently empty (topology is unaffected)",
			Fix: []string{
				"Create the extension in the postgres maintenance DB (the one the collector probes):",
				"  psql \"host=<host> port=<port> user=<user> dbname=postgres\" -c 'CREATE EXTENSION pg_stat_statements;'",
				cnpgStatStatementsNote,
			},
		}
	}
	return Result{
		Name:     "pg_stat_statements extension",
		Severity: Fail,
		Detail:   "pg_stat_statements extension not created in the `postgres` maintenance database (the collector probes it there for the workload capability)",
		Fix: []string{
			"Create the extension in the postgres maintenance DB:",
			"  psql -d postgres -c 'CREATE EXTENSION pg_stat_statements;'",
			cnpgStatStatementsNote,
		},
	}
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

// CheckTopologyGrants warns if the role cannot read every user table. The
// topology scrape uses pg_dump, which LOCKs each table IN ACCESS SHARE MODE and
// reads its definition; without read access the scrape fails and the topology
// view stays empty (metrics are unaffected). pg_read_all_stats is NOT enough --
// it grants stats, not table data.
func CheckTopologyGrants(ctx context.Context, ins Inspector) Result {
	n, err := ins.UnreadableUserTables(ctx)
	if err != nil {
		return Result{Name: "topology read permission", Severity: Warn, Detail: err.Error()}
	}
	if n > 0 {
		return Result{
			Name:     "topology read permission",
			Severity: Warn,
			Detail:   fmt.Sprintf("%d user table(s) not readable -- the pg_dump topology scrape will fail (metrics unaffected)", n),
			Fix: []string{
				"Grant the collector role read access to all data (Postgres 14+):",
				"  GRANT pg_read_all_data TO <your_role>;",
				"(Pre-14: GRANT SELECT on the relevant schemas/tables.)",
			},
		}
	}
	return Result{Name: "topology read permission", Severity: OK, Detail: "all user tables readable"}
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
				cnpgParametersNote,
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
				cnpgParametersNote,
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

// rowQuerier is the minimal slice of *pgx.Conn the adapter needs: run a query
// that returns a single row. Narrowing the concrete *pgx.Conn to this port lets
// the adapter's row-to-value mapping be unit-tested against a fake row source
// with no live database. *pgx.Conn satisfies it directly.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type pgxInspector struct {
	conn rowQuerier
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

// MaintenanceDBHasExtension checks for the extension in the `postgres`
// maintenance database -- the DB the collector's capability probe connects to,
// which may differ from the target DB this preflight is connected to. It opens a
// short-lived connection to the maintenance DB derived from the DSN.
func (p *pgxInspector) MaintenanceDBHasExtension(ctx context.Context, name string) (bool, error) {
	conn, err := pgx.Connect(ctx, maintenanceDSN(p.dsn))
	if err != nil {
		return false, fmt.Errorf("connect to postgres maintenance DB: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var present bool
	err = conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", name,
	).Scan(&present)
	return present, err
}

func (p *pgxInspector) CanReadStats(ctx context.Context) error {
	var count int
	return p.conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_statements LIMIT 1").Scan(&count)
}

// UnreadableUserTables counts user tables (excluding catalogs) the current role
// lacks SELECT on -- i.e. tables the pg_dump topology scrape can't read.
func (p *pgxInspector) UnreadableUserTables(ctx context.Context) (int, error) {
	var n int
	err := p.conn.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_class c
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND ns.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND ns.nspname NOT LIKE 'pg_temp%'
		  AND NOT has_table_privilege(current_user, c.oid, 'SELECT')
	`).Scan(&n)
	return n, err
}

// quoteIdent wraps an identifier in double quotes. The package only passes a
// fixed set of literal parameter names; this is defense-in-depth.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// maintenanceDSN returns dsn with its database set to `postgres` -- the
// maintenance DB the collector's capability probe connects to. Falls back to the
// original dsn if it can't be parsed as a URL.
func maintenanceDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return dsn
	}
	u.Path = "/postgres"
	return u.String()
}
