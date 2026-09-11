package collector

import (
	"reflect"
	"strings"
	"testing"
)

func TestCommandsFor(t *testing.T) {
	all := []string{CmdExecuteQuery, CmdExplain}

	// Empty request -> all supported, in catalog order.
	if got := CommandsFor("postgres", nil); !reflect.DeepEqual(got, all) {
		t.Errorf("empty request = %v, want all %v", got, all)
	}
	// A subset is preserved but reordered to catalog order and de-duped.
	if got := CommandsFor("postgres", []string{"explain", "explain", "execute_query"}); !reflect.DeepEqual(got, all) {
		t.Errorf("subset = %v, want catalog-ordered %v", got, all)
	}
	// Unknown commands are dropped (engine clamping).
	if got := CommandsFor("postgres", []string{"explain", "drop_table"}); !reflect.DeepEqual(got, []string{CmdExplain}) {
		t.Errorf("clamp = %v, want [explain]", got)
	}
	// Whitespace around a value is tolerated.
	if got := CommandsFor("postgres", []string{" execute_query "}); !reflect.DeepEqual(got, []string{CmdExecuteQuery}) {
		t.Errorf("trim = %v, want [execute_query]", got)
	}
}

func TestAwsComponent_CommandsEmitted(t *testing.T) {
	t.Run("carries the granted commands in order", func(t *testing.T) {
		got := awsComponent(AwsTarget{
			Name: "db", InstanceID: "db", Host: "h", Port: 5432,
			Commands: []string{CmdExecuteQuery, CmdExplain},
		}, "us-east-2")
		if !reflect.DeepEqual(got.Commands, []string{CmdExecuteQuery, CmdExplain}) {
			t.Errorf("commands = %v, want [execute_query explain]", got.Commands)
		}
	})

	t.Run("omits commands entirely when none granted", func(t *testing.T) {
		got := awsComponent(AwsTarget{Name: "db", InstanceID: "db", Host: "h", Port: 5432}, "us-east-2")
		if len(got.Commands) != 0 {
			t.Errorf("want no commands, got %v", got.Commands)
		}
		// `commands` is omitempty, so an ungranted component must not appear in
		// the rendered TOML at all — the collector reads that as "inherit the
		// global default", which is not the same as an empty list.
		rendered, err := Config{Component: []Component{got}}.Render()
		if err != nil {
			t.Fatal(err)
		}
		// The per-component key, not the global [commands] table.
		if strings.Contains(rendered, "commands = ") {
			t.Errorf("a target with no commands should render no commands key:\n%s", rendered)
		}
	})
}
