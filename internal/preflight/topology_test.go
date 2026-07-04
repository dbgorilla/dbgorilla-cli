package preflight

import (
	"context"
	"errors"
	"testing"
)

// fakePreflightInspector is a minimal Inspector for exercising the check logic
// without a live database. Only the fields a given test needs are set.
type fakePreflightInspector struct {
	unreadable    int
	unreadableErr error
}

func (f fakePreflightInspector) Summary() string { return "fake" }
func (f fakePreflightInspector) ServerVersionNum(context.Context) (string, error) {
	return "160000", nil
}
func (f fakePreflightInspector) ShowParam(_ context.Context, name string) (string, error) {
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
func (f fakePreflightInspector) HasExtension(context.Context, string) (bool, error) { return true, nil }
func (f fakePreflightInspector) CanReadStats(context.Context) error                 { return nil }
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
