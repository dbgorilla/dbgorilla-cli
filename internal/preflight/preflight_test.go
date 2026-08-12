package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// bg is a tiny helper to keep the check-call sites terse.
func bg() context.Context { return context.Background() }

// --- Report aggregators ---------------------------------------------------

func TestReport_Failed(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want bool
	}{
		{"has a fail", Report{Results: []Result{{Severity: OK}, {Severity: Fail}}}, true},
		{"only ok and warn", Report{Results: []Result{{Severity: OK}, {Severity: Warn}}}, false},
		{"empty", Report{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.Failed(); got != tc.want {
				t.Errorf("Failed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReport_HasWarnings(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want bool
	}{
		{"has a warn", Report{Results: []Result{{Severity: OK}, {Severity: Warn}}}, true},
		{"only ok and fail", Report{Results: []Result{{Severity: OK}, {Severity: Fail}}}, false},
		{"empty", Report{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.HasWarnings(); got != tc.want {
				t.Errorf("HasWarnings() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- CheckServerVersion ---------------------------------------------------

func TestCheckServerVersion_OK(t *testing.T) {
	r := CheckServerVersion(bg(), fakePreflightInspector{version: "160000"})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
	if r.Detail != "Postgres 16" {
		t.Errorf("detail = %q, want Postgres 16", r.Detail)
	}
}

func TestCheckServerVersion_QueryError(t *testing.T) {
	r := CheckServerVersion(bg(), fakePreflightInspector{versionErr: errors.New("boom")})
	if r.Severity != Fail {
		t.Fatalf("want Fail on query error, got %s", r.Severity)
	}
	if r.Detail != "boom" {
		t.Errorf("detail = %q, want the underlying error", r.Detail)
	}
}

func TestCheckServerVersion_UnparseableFormat(t *testing.T) {
	r := CheckServerVersion(bg(), fakePreflightInspector{version: "not-a-number"})
	if r.Severity != Fail {
		t.Fatalf("want Fail on bad format, got %s", r.Severity)
	}
	if !strings.Contains(r.Detail, "unexpected version format") {
		t.Errorf("detail = %q, want an unexpected-format message", r.Detail)
	}
}

func TestCheckServerVersion_TooOld(t *testing.T) {
	r := CheckServerVersion(bg(), fakePreflightInspector{version: "120005"})
	if r.Severity != Fail {
		t.Fatalf("want Fail for pg12, got %s", r.Severity)
	}
	if !strings.Contains(r.Detail, "Postgres 12") || len(r.Fix) == 0 {
		t.Errorf("expected pg12 detail + upgrade fix, got detail=%q fix=%v", r.Detail, r.Fix)
	}
}

func TestCheckServerVersion_BoundaryPG13OK(t *testing.T) {
	// 13 is the minimum supported major; it must pass.
	r := CheckServerVersion(bg(), fakePreflightInspector{version: "130000"})
	if r.Severity != OK {
		t.Fatalf("pg13 should be OK, got %s (%s)", r.Severity, r.Detail)
	}
}

// --- CheckSharedPreload ---------------------------------------------------

func TestCheckSharedPreload_OK(t *testing.T) {
	r := CheckSharedPreload(bg(), fakePreflightInspector{})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
}

func TestCheckSharedPreload_QueryError(t *testing.T) {
	r := CheckSharedPreload(bg(), fakePreflightInspector{paramErr: errors.New("nope")})
	if r.Severity != Fail {
		t.Fatalf("want Fail on query error, got %s", r.Severity)
	}
}

// Missing pg_stat_statements warns, it does not block: the collector still
// reports schema topology without it, so blocking made a stock local Postgres
// un-onboardable behind an ALTER SYSTEM and a database restart.
func TestCheckSharedPreload_NotLoadedWarnsNotFails(t *testing.T) {
	r := CheckSharedPreload(bg(), fakePreflightInspector{
		params: map[string]string{"shared_preload_libraries": "auto_explain"},
	})
	if r.Severity != Warn {
		t.Fatalf("want Warn when lib absent, got %s", r.Severity)
	}
	if len(r.Fix) == 0 {
		t.Error("expected a copy-pastable ALTER SYSTEM / RESTART fix")
	}
	if !strings.Contains(r.Detail, "query-performance") {
		t.Errorf("detail should say what is actually lost, got %q", r.Detail)
	}
}

// The remediation for a connection failure must point the RIGHT WAY: a server
// with no TLS needs less TLS, not more.
func TestConnectFix_TLSDirection(t *testing.T) {
	noTLS := strings.Join(tlsAwareConnectFix(errors.New(
		`failed to connect: server does not support SSL, but SSL was required`)), " ")
	if !strings.Contains(noTLS, "--ssl-mode disable") {
		t.Errorf("a server without TLS must be told to use disable, got: %s", noTLS)
	}
	if strings.Contains(noTLS, "set --ssl-mode require (or verify-full)") {
		t.Errorf("must not advise MORE TLS against a server that has none: %s", noTLS)
	}

	generic := strings.Join(tlsAwareConnectFix(errors.New("connection refused")), " ")
	if !strings.Contains(generic, "disable") {
		t.Errorf("generic advice should still name every mode including disable, got: %s", generic)
	}
}

// --- CheckExtension -------------------------------------------------------

func TestCheckWorkload_ConnectFailureWarns(t *testing.T) {
	// Unreachable DB -> Warn (never a hard error), named "workload".
	r := CheckWorkload(bg(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if r.Severity != Warn {
		t.Fatalf("want Warn on connect failure, got %s (%s)", r.Severity, r.Detail)
	}
	if r.Name != "workload" {
		t.Errorf("want Name=workload, got %q", r.Name)
	}
}

func TestMaintenanceDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5433/appdb?sslmode=disable": "postgres://u:p@h:5433/postgres?sslmode=disable",
		"postgres://u:p@h:5432/postgres":              "postgres://u:p@h:5432/postgres",
		"not a url":                                   "not a url", // unparseable -> unchanged
	}
	for in, want := range cases {
		if got := maintenanceDSN(in); got != want {
			t.Errorf("maintenanceDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckExtension_OK(t *testing.T) {
	// Present in the postgres maintenance DB -> OK (workload capability on).
	r := CheckExtension(bg(), fakePreflightInspector{})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
}

func TestCheckExtension_MaintenanceProbeErrorWarns(t *testing.T) {
	// Can't reach the postgres maintenance DB -> Warn, not a hard fail.
	r := CheckExtension(bg(), fakePreflightInspector{maintExtErr: errors.New("boom")})
	if r.Severity != Warn {
		t.Fatalf("want Warn when the maintenance-DB probe errors, got %s", r.Severity)
	}
}

func TestCheckExtension_TargetOnlyDivergenceFails(t *testing.T) {
	// The dbgorilla-cli#14 trap: extension is in the target DB but NOT in the
	// `postgres` maintenance DB the collector probes -> Fail with the specific
	// silent-empty-workload diagnosis, not the generic message.
	r := CheckExtension(bg(), fakePreflightInspector{maintExtMissing: true, extMissing: false})
	if r.Severity != Fail {
		t.Fatalf("want Fail on maintenance-DB/target divergence, got %s", r.Severity)
	}
	if !strings.Contains(r.Detail, "maintenance database") || !strings.Contains(r.Detail, "target database") {
		t.Errorf("expected a divergence-specific detail, got %q", r.Detail)
	}
	joined := strings.Join(r.Fix, " ")
	if !strings.Contains(joined, "dbname=postgres") {
		t.Errorf("expected a fix that creates the extension in the postgres DB, got %v", r.Fix)
	}
}

func TestCheckExtension_MissingEverywhereFails(t *testing.T) {
	// Not in the maintenance DB and not in the target DB -> Fail, fix points at
	// the postgres maintenance DB.
	r := CheckExtension(bg(), fakePreflightInspector{maintExtMissing: true, extMissing: true})
	if r.Severity != Fail {
		t.Fatalf("want Fail when extension missing everywhere, got %s", r.Severity)
	}
	if !strings.Contains(strings.Join(r.Fix, " "), "postgres") {
		t.Errorf("expected the fix to reference the postgres maintenance DB, got %v", r.Fix)
	}
}

// --- CheckStatsGrants -----------------------------------------------------

func TestCheckStatsGrants_OK(t *testing.T) {
	r := CheckStatsGrants(bg(), fakePreflightInspector{})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
}

func TestCheckStatsGrants_PermissionDenied(t *testing.T) {
	// A "permission"/"denied" error is a hard Fail with a GRANT fix.
	for _, msg := range []string{"permission denied for relation", "access denied"} {
		r := CheckStatsGrants(bg(), fakePreflightInspector{canReadErr: errors.New(msg)})
		if r.Severity != Fail {
			t.Fatalf("msg %q: want Fail, got %s", msg, r.Severity)
		}
		if len(r.Fix) == 0 {
			t.Errorf("msg %q: expected a GRANT pg_read_all_stats fix", msg)
		}
	}
}

func TestCheckStatsGrants_OtherErrorWarns(t *testing.T) {
	// A non-permission error (e.g. relation missing) is a soft Warn, not a Fail.
	r := CheckStatsGrants(bg(), fakePreflightInspector{
		canReadErr: errors.New(`relation "pg_stat_statements" does not exist`),
	})
	if r.Severity != Warn {
		t.Fatalf("want Warn on non-permission error, got %s", r.Severity)
	}
	if !strings.Contains(r.Detail, "does not exist") {
		t.Errorf("detail should surface the raw error, got %q", r.Detail)
	}
}

// --- CheckTrackIOTiming ---------------------------------------------------

func TestCheckTrackIOTiming_OK(t *testing.T) {
	r := CheckTrackIOTiming(bg(), fakePreflightInspector{})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
}

func TestCheckTrackIOTiming_QueryErrorWarns(t *testing.T) {
	r := CheckTrackIOTiming(bg(), fakePreflightInspector{paramErr: errors.New("boom")})
	if r.Severity != Warn {
		t.Fatalf("want Warn on query error, got %s", r.Severity)
	}
}

func TestCheckTrackIOTiming_Off(t *testing.T) {
	r := CheckTrackIOTiming(bg(), fakePreflightInspector{
		params: map[string]string{"track_io_timing": "off"},
	})
	if r.Severity != Warn {
		t.Fatalf("want Warn when off, got %s", r.Severity)
	}
	if len(r.Fix) == 0 {
		t.Error("expected an ALTER SYSTEM / pg_reload_conf fix")
	}
}

// --- CheckTrackFunctions --------------------------------------------------

func TestCheckTrackFunctions_OK(t *testing.T) {
	r := CheckTrackFunctions(bg(), fakePreflightInspector{})
	if r.Severity != OK || r.Detail != "pl" {
		t.Fatalf("want OK/pl, got %s/%q", r.Severity, r.Detail)
	}
}

func TestCheckTrackFunctions_NonNoneValueIsOK(t *testing.T) {
	// Any value other than "none" is acceptable and echoed back in the detail.
	r := CheckTrackFunctions(bg(), fakePreflightInspector{
		params: map[string]string{"track_functions": "all"},
	})
	if r.Severity != OK || r.Detail != "all" {
		t.Fatalf("want OK/all, got %s/%q", r.Severity, r.Detail)
	}
}

func TestCheckTrackFunctions_QueryErrorWarns(t *testing.T) {
	r := CheckTrackFunctions(bg(), fakePreflightInspector{paramErr: errors.New("boom")})
	if r.Severity != Warn {
		t.Fatalf("want Warn on query error, got %s", r.Severity)
	}
}

func TestCheckTrackFunctions_None(t *testing.T) {
	r := CheckTrackFunctions(bg(), fakePreflightInspector{
		params: map[string]string{"track_functions": "none"},
	})
	if r.Severity != Warn {
		t.Fatalf("want Warn when none, got %s", r.Severity)
	}
	if len(r.Fix) == 0 {
		t.Error("expected a track_functions fix")
	}
}

// --- helpers --------------------------------------------------------------

func TestContainsLib(t *testing.T) {
	tests := []struct {
		libs, needle string
		want         bool
	}{
		{"pg_stat_statements", "pg_stat_statements", true},
		{"auto_explain, pg_stat_statements", "pg_stat_statements", true}, // trims surrounding space
		{"pg_stat_statements,auto_explain", "pg_stat_statements", true},  // first entry
		{"auto_explain", "pg_stat_statements", false},
		{"", "pg_stat_statements", false},
	}
	for _, tc := range tests {
		if got := ContainsLib(tc.libs, tc.needle); got != tc.want {
			t.Errorf("ContainsLib(%q, %q) = %v, want %v", tc.libs, tc.needle, got, tc.want)
		}
	}
}

func TestDSNSummary(t *testing.T) {
	tests := []struct {
		dsn, want string
	}{
		{"postgres://user:pass@db.example:5432/app", "connected to db.example:5432/app"},
		{"postgresql://db.example/app", "connected to db.example/app"}, // no credentials
		{"host=localhost dbname=app user=ro", "connected"},             // keyword/value form, no scheme
	}
	for _, tc := range tests {
		if got := DSNSummary(tc.dsn); got != tc.want {
			t.Errorf("DSNSummary(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
	// Credentials must never appear in the summary.
	if got := DSNSummary("postgres://user:secret@h/db"); strings.Contains(got, "secret") {
		t.Errorf("DSNSummary leaked credentials: %q", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("track_io_timing"); got != `"track_io_timing"` {
		t.Errorf("quoteIdent plain = %q", got)
	}
	// Embedded double-quotes are doubled (SQL identifier escaping).
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent escaped = %q", got)
	}
}

// --- Run / RunWith --------------------------------------------------------

func TestRun_ConnectFailure(t *testing.T) {
	// A malformed DSN fails to parse offline (no network, no DB), which is the
	// deterministic way to drive Run's connect short-circuit.
	rep := Run(bg(), "://not a dsn::::")
	if len(rep.Results) != 1 {
		t.Fatalf("connect failure should short-circuit to one result, got %d", len(rep.Results))
	}
	got := rep.Results[0]
	if got.Name != "connect" || got.Severity != Fail {
		t.Fatalf("want a failing connect result, got %+v", got)
	}
	if len(got.Fix) == 0 {
		t.Error("connect failure should carry troubleshooting hints")
	}
	if !rep.Failed() {
		t.Error("report should report Failed() == true")
	}
}

func TestRunWith_AllHealthy(t *testing.T) {
	rep := RunWith(bg(), fakePreflightInspector{summary: "connected to h/db"})
	if rep.Failed() {
		t.Errorf("healthy inspector should not fail: %+v", rep.Results)
	}
	if rep.HasWarnings() {
		t.Errorf("healthy inspector should not warn: %+v", rep.Results)
	}
	// connect + 7 checks.
	if len(rep.Results) != 8 {
		t.Fatalf("want 8 results, got %d", len(rep.Results))
	}
	if rep.Results[0].Name != "connect" || rep.Results[0].Detail != "connected to h/db" {
		t.Errorf("first result should echo the inspector summary, got %+v", rep.Results[0])
	}
}

func TestRunWith_MixedFailAndWarn(t *testing.T) {
	rep := RunWith(bg(), fakePreflightInspector{
		version:    "120000",                                    // Fail: too old
		unreadable: 2,                                           // Warn: topology grants
		params:     map[string]string{"track_io_timing": "off"}, // Warn
	})
	if !rep.Failed() {
		t.Error("expected Failed() == true with an old server version")
	}
	if !rep.HasWarnings() {
		t.Error("expected HasWarnings() == true with off track_io_timing")
	}
}

// --- pgx adapter row-mapping (via the rowQuerier seam, no live DB) ---------

// fakeRow implements pgx.Row by delegating Scan to a closure.
type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// fakeQuerier implements rowQuerier and records the last SQL + args it saw.
type fakeQuerier struct {
	row      pgx.Row
	lastSQL  string
	lastArgs []any
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.lastSQL = sql
	q.lastArgs = args
	return q.row
}

// assignRow returns a fakeRow that writes vals into the scan destinations in
// order, supporting the *string/*bool/*int destinations the adapter uses.
func assignRow(vals ...any) fakeRow {
	return fakeRow{scan: func(dest ...any) error {
		for i := range dest {
			switch d := dest[i].(type) {
			case *string:
				*d = vals[i].(string)
			case *bool:
				*d = vals[i].(bool)
			case *int:
				*d = vals[i].(int)
			default:
				return errors.New("unexpected scan destination type")
			}
		}
		return nil
	}}
}

func errRow(err error) fakeRow {
	return fakeRow{scan: func(...any) error { return err }}
}

func TestPgxInspector_Summary(t *testing.T) {
	// Summary needs no connection; it only formats the DSN.
	ins := &pgxInspector{dsn: "postgres://user:pass@h:5432/db"}
	if got := ins.Summary(); got != "connected to h:5432/db" {
		t.Errorf("Summary() = %q", got)
	}
}

func TestPgxInspector_ServerVersionNum(t *testing.T) {
	q := &fakeQuerier{row: assignRow("160000")}
	ins := &pgxInspector{conn: q}
	v, err := ins.ServerVersionNum(bg())
	if err != nil || v != "160000" {
		t.Fatalf("got (%q, %v), want (160000, nil)", v, err)
	}
	if q.lastSQL != "SHOW server_version_num" {
		t.Errorf("unexpected SQL: %q", q.lastSQL)
	}

	q2 := &fakeQuerier{row: errRow(errors.New("boom"))}
	if _, err := (&pgxInspector{conn: q2}).ServerVersionNum(bg()); err == nil {
		t.Error("expected scan error to propagate")
	}
}

func TestPgxInspector_ShowParam(t *testing.T) {
	q := &fakeQuerier{row: assignRow("on")}
	ins := &pgxInspector{conn: q}
	v, err := ins.ShowParam(bg(), "track_io_timing")
	if err != nil || v != "on" {
		t.Fatalf("got (%q, %v), want (on, nil)", v, err)
	}
	// The parameter name must be passed through quoteIdent.
	if q.lastSQL != `SHOW "track_io_timing"` {
		t.Errorf("SQL = %q, want a quoted identifier", q.lastSQL)
	}
}

func TestPgxInspector_HasExtension(t *testing.T) {
	q := &fakeQuerier{row: assignRow(true)}
	ins := &pgxInspector{conn: q}
	present, err := ins.HasExtension(bg(), "pg_stat_statements")
	if err != nil || !present {
		t.Fatalf("got (%v, %v), want (true, nil)", present, err)
	}
	// The extension name is bound as a query parameter, not interpolated.
	if len(q.lastArgs) != 1 || q.lastArgs[0] != "pg_stat_statements" {
		t.Errorf("args = %v, want [pg_stat_statements]", q.lastArgs)
	}

	q2 := &fakeQuerier{row: errRow(errors.New("boom"))}
	if _, err := (&pgxInspector{conn: q2}).HasExtension(bg(), "x"); err == nil {
		t.Error("expected scan error to propagate")
	}
}

func TestPgxInspector_CanReadStats(t *testing.T) {
	q := &fakeQuerier{row: assignRow(1)}
	if err := (&pgxInspector{conn: q}).CanReadStats(bg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q2 := &fakeQuerier{row: errRow(errors.New("permission denied"))}
	if err := (&pgxInspector{conn: q2}).CanReadStats(bg()); err == nil {
		t.Error("expected permission error to propagate")
	}
}

func TestPgxInspector_UnreadableUserTables(t *testing.T) {
	q := &fakeQuerier{row: assignRow(3)}
	n, err := (&pgxInspector{conn: q}).UnreadableUserTables(bg())
	if err != nil || n != 3 {
		t.Fatalf("got (%d, %v), want (3, nil)", n, err)
	}
	q2 := &fakeQuerier{row: errRow(errors.New("boom"))}
	if _, err := (&pgxInspector{conn: q2}).UnreadableUserTables(bg()); err == nil {
		t.Error("expected scan error to propagate")
	}
}
