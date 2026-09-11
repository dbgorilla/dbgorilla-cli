package collector

import (
	"reflect"
	"strings"
	"testing"
)

// The pieces both cloud targets share.

func TestLogCursor_SuppressesOnlyWhatItAlreadyPrinted(t *testing.T) {
	c := newLogCursor(100)
	if !c.accept("a", 100) || !c.accept("b", 100) {
		t.Fatal("fresh entries at the start instant must print")
	}
	// The next poll restarts at the same (inclusive) instant and sees them again.
	if c.accept("a", 100) || c.accept("b", 100) {
		t.Fatal("entries printed at the cursor instant must not print twice")
	}
	if !c.accept("c", 100) {
		t.Fatal("a new same-instant entry (written after the last read) must print — ts+1 cursors would drop it")
	}
	// Advancing forgets the older instant's ids: bounded memory.
	if !c.accept("d", 200) || len(c.seen) != 1 || c.newest != 200 {
		t.Fatalf("advance: seen=%v newest=%d", c.seen, c.newest)
	}
	if !c.accept("", 200) {
		t.Fatal("an entry without an id can never be deduped, so it prints")
	}
}

func TestBaseConfig_IsTheOneIdentityBlock(t *testing.T) {
	// The three renderers (docker, aws, gcp) share this; a secret rendered here
	// would leak through all of them at once.
	cfg := baseConfig("agent-1", "tenant-1", Endpoints{OpampBaseURL: "wss://x"}, true)
	if cfg.Dbgorilla.Secret != "${"+SecretEnv+"}" || cfg.Dbgorilla.AgentID != "agent-1" || cfg.Dbgorilla.OpampBaseURL != "wss://x" {
		t.Fatalf("identity block: %+v", cfg.Dbgorilla)
	}
	if !cfg.Commands.Enabled || cfg.Topology.Interval != "60s" {
		t.Fatalf("globals: %+v %+v", cfg.Commands, cfg.Topology)
	}
}

func TestAmbiguousTargetError_LabelsEveryKind(t *testing.T) {
	err := &AmbiguousTargetError{Choices: []TargetChoice{
		{ID: "pg", ProviderType: "aws_rds"}, {ID: "au", ProviderType: "aws_aurora"},
		{ID: "cs", ProviderType: "cloud_sql"}, {ID: "ad/ad-primary", ProviderType: "alloydb"},
	}}
	for _, want := range []string{"pg (RDS)", "au (Aurora)", "cs (Cloud SQL)", "ad/ad-primary (AlloyDB)", "--db-instance-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should carry %q, got: %s", want, err)
		}
	}
}

// ResolveCommands is generic over the cloud targets; the gcp side adds an
// engine the catalog does not cover.
func TestResolveCommands_GcpTargetsClampToTheirEngine(t *testing.T) {
	t.Run("postgres gets the full catalog by default", func(t *testing.T) {
		targets := []GcpTarget{{InstanceID: "pg", Engine: "postgres"}}
		if !ResolveCommands(targets, CommandRequest{}, nil) {
			t.Error("want enabled")
		}
		if !reflect.DeepEqual(targets[0].Commands, CommandCatalog("postgres")) {
			t.Errorf("commands = %v", targets[0].Commands)
		}
	})
	t.Run("mysql has no catalog, so analysis stays off for it", func(t *testing.T) {
		targets := []GcpTarget{{InstanceID: "my", Engine: "mysql"}}
		if ResolveCommands(targets, CommandRequest{Explicit: true, Commands: []string{"explain"}}, nil) {
			t.Error("an engine without a catalog cannot enable analysis")
		}
		if len(targets[0].Commands) != 0 {
			t.Errorf("commands = %v, want none", targets[0].Commands)
		}
	})
	t.Run("forced off clears a mixed set", func(t *testing.T) {
		targets := []GcpTarget{{Engine: "postgres", Commands: []string{"explain"}}, {Engine: "mysql"}}
		if ResolveCommands(targets, CommandRequest{ForcedOff: true}, nil) {
			t.Error("want disabled")
		}
		if targets[0].Commands != nil {
			t.Errorf("commands = %v, want cleared", targets[0].Commands)
		}
	})
}

func TestGcpDatabaseUserFor(t *testing.T) {
	sa := "dbgorilla-collector@acme-prod.iam.gserviceaccount.com"
	if got := GcpDatabaseUserFor(sa, "postgres"); got != "dbgorilla-collector@acme-prod.iam" {
		t.Errorf("postgres IAM username: %q", got)
	}
	if got := GcpDatabaseUserFor(sa, "mysql"); got != "dbgorilla-collector" {
		t.Errorf("mysql IAM username: %q", got)
	}
	// Something that is not a service-account email passes through unchanged.
	if got := GcpDatabaseUserFor("alice@example.com", "postgres"); got != "alice@example.com" {
		t.Errorf("plain user mangled: %q", got)
	}
}
