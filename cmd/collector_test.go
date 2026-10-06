package cmd

import (
	"reflect"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// commandsTestCmd builds a command with the query-analysis flags. Tests run
// without a TTY, so interactiveSelectable is false — resolveCommands takes its
// non-interactive branches (no checklist prompt).
func commandsTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("enable-commands", false, "")
	c.Flags().String("commands", "", "")
	c.Flags().Bool("yes", false, "")
	return c
}

func TestResolveCommands_HardOff(t *testing.T) {
	cases := map[string]func(*cobra.Command){
		"--enable-commands=false": func(c *cobra.Command) { _ = c.Flags().Set("enable-commands", "false") },
		`--commands=""`:           func(c *cobra.Command) { _ = c.Flags().Set("commands", "") }, // Set marks it changed
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := commandsTestCmd()
			setup(c)
			// Seed commands, or "cleared" is indistinguishable from "never set"
			// and the assertion below cannot fail.
			targets := []collector.AwsTarget{
				{Name: "a", Commands: []string{collector.CmdExplain}},
				{Name: "b", Commands: []string{collector.CmdExecuteQuery, collector.CmdExplain}},
			}
			if resolveCommands(c, targets, awsTargetLabel) {
				t.Error("a hard off should return disabled")
			}
			for _, tg := range targets {
				if len(tg.Commands) > 0 {
					t.Errorf("a hard off should clear commands, got %v", tg.Commands)
				}
			}
		})
	}
}

func TestResolveCommands_ExplainByDefault(t *testing.T) {
	// Non-interactive with no flag: the default, and the gate is on for it.
	c := commandsTestCmd()
	targets := []collector.AwsTarget{{Name: "a"}}
	if !resolveCommands(c, targets, awsTargetLabel) {
		t.Error("no flag should turn commands on for the default")
	}
	if !reflect.DeepEqual(targets[0].Commands, []string{collector.CmdExplain, collector.CmdCollectStatistics}) {
		t.Errorf("no flag should grant explain and collect_statistics, got %v", targets[0].Commands)
	}
}

// Paths without a per-database checklist (local Docker, Instaclustr,
// helm-values) read the flags through flagCommands.
func TestFlagCommands(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]string
		want  []string
	}{
		{"no flag grants explain and collect_statistics", nil, []string{collector.CmdExplain, collector.CmdCollectStatistics}},
		{"enable-commands grants all", map[string]string{"enable-commands": "true"}, collector.CommandCatalog("postgres")},
		{"enable-commands=false grants none", map[string]string{"enable-commands": "false"}, nil},
		{"empty --commands grants none", map[string]string{"commands": ""}, nil},
		{"--commands picks a subset", map[string]string{"commands": "execute_query"}, []string{collector.CmdExecuteQuery}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := commandsTestCmd()
			for k, v := range tc.flags {
				_ = c.Flags().Set(k, v)
			}
			if got := flagCommands(c, "postgres"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveCommands_EnableCommandsGrantsTheCatalog(t *testing.T) {
	// --enable-commands turns on every command the engine supports, and the
	// gate is implicitly on because a database ended up with commands.
	c := commandsTestCmd()
	_ = c.Flags().Set("enable-commands", "true")
	targets := []collector.AwsTarget{{Name: "a"}}
	if !resolveCommands(c, targets, awsTargetLabel) {
		t.Error("--enable-commands should turn commands on")
	}
	if len(targets[0].Commands) != len(collector.CommandCatalog("postgres")) {
		t.Errorf("--enable-commands should allow the full catalog, got %v", targets[0].Commands)
	}
}

func TestResolveCommands_FlagSubsetAndConfigClamp(t *testing.T) {
	// An explicit --commands subset applies to all databases.
	c := commandsTestCmd()
	_ = c.Flags().Set("commands", "explain")
	// b's configured command deliberately differs from --commands: if it were
	// also "explain", overwriting and retaining would produce the same result and
	// the assertion could not tell them apart.
	targets := []collector.AwsTarget{{Name: "a"}, {Name: "b", Commands: []string{"execute_query", "bogus"}}}
	if !resolveCommands(c, targets, awsTargetLabel) {
		t.Error("explicit commands should be enabled")
	}
	// --commands wins for a target without its own config commands...
	if len(targets[0].Commands) != 1 || targets[0].Commands[0] != collector.CmdExplain {
		t.Errorf("target a: want [explain], got %v", targets[0].Commands)
	}
	// ...and a target's own (config) commands are kept but clamped (bogus
	// dropped), rather than being replaced by --commands.
	if len(targets[1].Commands) != 1 || targets[1].Commands[0] != collector.CmdExecuteQuery {
		t.Errorf("target b: config commands should be kept+clamped, got %v", targets[1].Commands)
	}
}
