package collector

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPlanGrants(t *testing.T) {
	t.Run("password-auth databases need no IAM grant", func(t *testing.T) {
		plan := PlanGrants([]AwsTarget{
			{InstanceID: "a", AuthMethod: "password"},
			{InstanceID: "b", AuthMethod: "password"},
		}, nil)
		if len(plan.Targets) != 0 {
			t.Errorf("want nothing to grant, got %v", plan.Targets)
		}
	})

	t.Run("one target per server, IAM only", func(t *testing.T) {
		plan := PlanGrants([]AwsTarget{
			{InstanceID: "a", Databases: []string{"one"}},
			{InstanceID: "a", Databases: []string{"two"}}, // same server, second DB
			{InstanceID: "b", AuthMethod: "password"},
			{InstanceID: "c"},
		}, nil)
		var ids []string
		for _, tg := range plan.Targets {
			ids = append(ids, tg.InstanceID)
		}
		if !reflect.DeepEqual(ids, []string{"a", "c"}) {
			t.Errorf("want [a c], got %v", ids)
		}
	})

	t.Run("unreachable servers are reported, not dropped", func(t *testing.T) {
		probe := func(addr string) error {
			if strings.HasPrefix(addr, "private") {
				return errors.New("timeout")
			}
			return nil
		}
		plan := PlanGrants([]AwsTarget{
			{InstanceID: "public-db", Host: "public", Port: 5432},
			{InstanceID: "private-db", Host: "private", Port: 5432},
		}, probe)
		if len(plan.Targets) != 2 {
			t.Fatalf("both still need a grant, got %v", plan.Targets)
		}
		if !reflect.DeepEqual(plan.Unreachable, []string{"private-db"}) {
			t.Errorf("want [private-db] unreachable, got %v", plan.Unreachable)
		}
	})

	t.Run("nil probe skips the reachability check", func(t *testing.T) {
		plan := PlanGrants([]AwsTarget{{InstanceID: "a", Host: "h"}}, nil)
		if plan.Unreachable != nil {
			t.Errorf("want no probing, got %v", plan.Unreachable)
		}
	})
}

func TestAdminDSN(t *testing.T) {
	t.Run("an unset ssl_mode must not downgrade", func(t *testing.T) {
		dsn := AdminDSN("admin", "pw", AwsTarget{Host: "db.example", Port: 5432})
		if !strings.Contains(dsn, "sslmode=verify-full") {
			t.Errorf("want verify-full for an unset ssl_mode, got %q", dsn)
		}
		if strings.Contains(dsn, "sslmode=require") {
			t.Error("an admin credential must not ride an unvalidated connection")
		}
	})

	t.Run("an explicit ssl_mode is honored", func(t *testing.T) {
		dsn := AdminDSN("admin", "pw", AwsTarget{Host: "h", Port: 5432, SSLMode: "verify-ca"})
		if !strings.Contains(dsn, "sslmode=verify-ca") {
			t.Errorf("want the target's own mode, got %q", dsn)
		}
	})

	t.Run("missing port defaults to postgres", func(t *testing.T) {
		if got := TargetDial(AwsTarget{Host: "h"}); got != "h:5432" {
			t.Errorf("want h:5432, got %q", got)
		}
	})
}

// The precedence these cases pin used to be reachable only through a
// cobra.Command; it is now testable on its own.
func TestResolveCommands_Precedence(t *testing.T) {
	all := []string{CmdExecuteQuery, CmdExplain}

	t.Run("config commands win over the flag, and are clamped", func(t *testing.T) {
		targets := []AwsTarget{{Name: "a", Commands: []string{"execute_query", "bogus"}}}
		if !ResolveCommands(targets, CommandRequest{Explicit: true, Commands: []string{"explain"}}, nil) {
			t.Error("want enabled")
		}
		if !reflect.DeepEqual(targets[0].Commands, []string{CmdExecuteQuery}) {
			t.Errorf("config should win and clamp, got %v", targets[0].Commands)
		}
	})

	t.Run("an explicit flag applies to targets without their own", func(t *testing.T) {
		targets := []AwsTarget{{Name: "a"}}
		ResolveCommands(targets, CommandRequest{Explicit: true, Commands: []string{"explain"}}, nil)
		if !reflect.DeepEqual(targets[0].Commands, []string{CmdExplain}) {
			t.Errorf("want [explain], got %v", targets[0].Commands)
		}
	})

	t.Run("forced off clears everything and skips the prompt", func(t *testing.T) {
		targets := []AwsTarget{{Name: "a", Commands: all}, {Name: "b", Commands: all}}
		prompted := false
		if ResolveCommands(targets, CommandRequest{ForcedOff: true}, func(AwsTarget) []string {
			prompted = true
			return all
		}) {
			t.Error("a hard off should report disabled")
		}
		for _, tg := range targets {
			if len(tg.Commands) > 0 {
				t.Errorf("%s: commands should be cleared, got %v", tg.Name, tg.Commands)
			}
		}
		if prompted {
			t.Error("a hard off must not prompt")
		}
	})

	t.Run("no prompt means the full catalog", func(t *testing.T) {
		targets := []AwsTarget{{Name: "a"}}
		if !ResolveCommands(targets, CommandRequest{}, nil) {
			t.Error("want enabled")
		}
		if !reflect.DeepEqual(targets[0].Commands, all) {
			t.Errorf("want the full catalog, got %v", targets[0].Commands)
		}
	})

	t.Run("the gate is implicit: no commands anywhere means off", func(t *testing.T) {
		targets := []AwsTarget{{Name: "a"}, {Name: "b"}}
		if ResolveCommands(targets, CommandRequest{}, func(AwsTarget) []string { return nil }) {
			t.Error("unchecking every database should turn analysis off")
		}
	})
}
