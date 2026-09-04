package cmd

import (
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

func imageTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("image", collector.DefaultImage, "")
	return c
}

func TestResolveImage_DefaultWhenNoPreferredOrFlag(t *testing.T) {
	if img, _ := resolveImage(imageTestCmd(), &api.CollectorCredentials{}); img != collector.DefaultImage {
		t.Errorf("got %q, want default %q", img, collector.DefaultImage)
	}
}

func TestResolveImage_PreferredVersionWins(t *testing.T) {
	img, _ := resolveImage(imageTestCmd(), &api.CollectorCredentials{PreferredCollectorVersion: "0.2.0"})
	if want := collector.ImageForVersion("0.2.0"); img != want {
		t.Errorf("got %q, want %q", img, want)
	}
}

func TestResolveImage_ExplicitFlagOverridesPreferred(t *testing.T) {
	c := imageTestCmd()
	_ = c.Flags().Set("image", "myregistry/dbg-collector:custom") // marks the flag changed
	if img, _ := resolveImage(c, &api.CollectorCredentials{PreferredCollectorVersion: "0.2.0"}); img != "myregistry/dbg-collector:custom" {
		t.Errorf("explicit --image should win, got %q", img)
	}
}

// commandsTestCmd builds a command with the query-analysis flags. Tests run
// without a TTY, so interactiveSelectable is false — resolveCommands takes its
// non-interactive branches (no checklist prompt).
func commandsTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("enable-commands", true, "")
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

func TestResolveCommands_ImplicitGate(t *testing.T) {
	// No prompt, non-interactive: default is every command, and the gate is
	// implicitly on because a database ended up with commands.
	c := commandsTestCmd()
	targets := []collector.AwsTarget{{Name: "a"}}
	if !resolveCommands(c, targets, awsTargetLabel) {
		t.Error("default (all commands) should be implicitly enabled")
	}
	if len(targets[0].Commands) != 2 {
		t.Errorf("default should allow the full catalog, got %v", targets[0].Commands)
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
