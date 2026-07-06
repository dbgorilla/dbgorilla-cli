package preflight

import (
	"context"
	"errors"
	"testing"
)

// fakePreflightInspector is a minimal Inspector for exercising the check logic
// without a live database. Every field is optional: when a field is left at its
// zero value the method returns the "all healthy" default, so a test only sets
// what it wants to steer. This keeps the happy-path callers terse while letting
// other tests drive each error/degraded branch.
type fakePreflightInspector struct {
	summary         string            // Summary() override; "" -> "fake"
	version         string            // ServerVersionNum() override; "" -> "160000"
	versionErr      error             // ServerVersionNum() error
	params          map[string]string // ShowParam() overrides by name
	paramErr        error             // ShowParam() error (all names)
	extMissing      bool              // HasExtension() -> false when true
	hasExtErr       error             // HasExtension() error
	maintExtMissing bool              // MaintenanceDBHasExtension() -> false when true
	maintExtErr     error             // MaintenanceDBHasExtension() error
	canReadErr      error             // CanReadStats() error
	unreadable      int               // UnreadableUserTables() count
	unreadableErr   error             // UnreadableUserTables() error
}

func (f fakePreflightInspector) Summary() string {
	if f.summary != "" {
		return f.summary
	}
	return "fake"
}
func (f fakePreflightInspector) ServerVersionNum(context.Context) (string, error) {
	if f.versionErr != nil {
		return "", f.versionErr
	}
	if f.version != "" {
		return f.version, nil
	}
	return "160000", nil
}
func (f fakePreflightInspector) ShowParam(_ context.Context, name string) (string, error) {
	if f.paramErr != nil {
		return "", f.paramErr
	}
	if v, ok := f.params[name]; ok {
		return v, nil
	}
	switch name {
	case "shared_preload_libraries":
		return "pg_stat_statements", nil
	case "track_io_timing":
		return "on", nil
	case "track_functions":
		return "pl", nil
	}
	return "", nil
}
func (f fakePreflightInspector) HasExtension(context.Context, string) (bool, error) {
	if f.hasExtErr != nil {
		return false, f.hasExtErr
	}
	return !f.extMissing, nil
}
func (f fakePreflightInspector) MaintenanceDBHasExtension(context.Context, string) (bool, error) {
	if f.maintExtErr != nil {
		return false, f.maintExtErr
	}
	return !f.maintExtMissing, nil
}
func (f fakePreflightInspector) CanReadStats(context.Context) error { return f.canReadErr }
func (f fakePreflightInspector) UnreadableUserTables(context.Context) (int, error) {
	return f.unreadable, f.unreadableErr
}

func TestCheckTopologyGrants_allReadable(t *testing.T) {
	r := CheckTopologyGrants(context.Background(), fakePreflightInspector{unreadable: 0})
	if r.Severity != OK {
		t.Fatalf("want OK, got %s (%s)", r.Severity, r.Detail)
	}
}

func TestCheckTopologyGrants_someUnreadable(t *testing.T) {
	r := CheckTopologyGrants(context.Background(), fakePreflightInspector{unreadable: 3})
	if r.Severity != Warn {
		t.Fatalf("want Warn, got %s", r.Severity)
	}
	if len(r.Fix) == 0 {
		t.Error("expected a copy-pastable fix hint")
	}
}

func TestCheckTopologyGrants_queryError(t *testing.T) {
	r := CheckTopologyGrants(context.Background(), fakePreflightInspector{unreadableErr: errors.New("boom")})
	if r.Severity != Warn {
		t.Fatalf("want Warn on query error, got %s", r.Severity)
	}
}

func TestRunWith_includesTopologyCheck(t *testing.T) {
	rep := RunWith(context.Background(), fakePreflightInspector{unreadable: 0})
	for _, x := range rep.Results {
		if x.Name == "topology read permission" {
			return
		}
	}
	t.Error("RunWith is missing the 'topology read permission' check")
}
