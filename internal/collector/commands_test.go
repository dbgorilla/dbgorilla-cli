package collector

import (
	"reflect"
	"strings"
	"testing"
)

func TestCommandsFor(t *testing.T) {
	all := []string{CmdExecuteQuery, CmdExplain, CmdCollectStatistics}

	// Empty request -> all supported, in catalog order.
	if got := CommandsFor("postgres", nil); !reflect.DeepEqual(got, all) {
		t.Errorf("empty request = %v, want all %v", got, all)
	}
	// A subset is preserved but reordered to catalog order and de-duped.
	pair := []string{CmdExecuteQuery, CmdExplain}
	if got := CommandsFor("postgres", []string{"explain", "explain", "execute_query"}); !reflect.DeepEqual(got, pair) {
		t.Errorf("subset = %v, want catalog-ordered %v", got, pair)
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

// A database left with no commands must stay at none even when another
// database turns the gate on: the collector gives a component with no list of
// its own whatever [commands] allows, which is everything unless allowed = [].
func TestCloudConfigs_GateOnGrantsNothingByDefault(t *testing.T) {
	aws, err := awsConfigTOML("a", "t", "us-east-2", []AwsTarget{
		{Name: "on", InstanceID: "on", Host: "h", Commands: []string{CmdExplain}},
		{Name: "off", InstanceID: "off", Host: "h"},
	}, Endpoints{}, true)
	if err != nil {
		t.Fatal(err)
	}
	gcp, err := GcpConfigTOML("a", "t", []GcpTarget{
		{InstanceID: "on", Engine: "postgres", Commands: []string{CmdExplain}},
		{InstanceID: "off", Engine: "postgres"},
	}, Endpoints{}, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, rendered := range map[string]string{"aws": aws, "gcp": gcp} {
		conf, err := StrictParseConfig(rendered)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !conf.Commands.Enabled || conf.Commands.Allowed == nil || len(*conf.Commands.Allowed) != 0 {
			t.Errorf("%s: want enabled with allowed = [], got %+v", name, conf.Commands)
		}
	}

	off, err := awsConfigTOML("a", "t", "us-east-2", []AwsTarget{{Name: "off", InstanceID: "off", Host: "h"}}, Endpoints{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "allowed") {
		t.Errorf("with the gate off there is nothing to narrow:\n%s", off)
	}
}

// A local Docker config carries the database's own list, and turns the gate on
// with an empty allowed set so nothing else is inherited.
func TestBuildRendersTargetCommands(t *testing.T) {
	out, err := Build("a", "t", Target{Name: "n", Host: "h", Port: 5432, User: "u", Commands: DefaultCommands("postgres")}, Endpoints{}).Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`commands = ["explain", "collect_statistics"]`, "enabled = true", "allowed = []"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	none, err := Build("a", "t", Target{Name: "n", Host: "h", Port: 5432, User: "u"}, Endpoints{}).Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none, "enabled = false") || strings.Contains(none, "commands = [") {
		t.Errorf("no commands should render the gate off:\n%s", none)
	}
}

// Both engines support collect_statistics, so both default to it: without the
// optimizer statistics a recommendation cannot be tested before it is made.
func TestDefaultCommandsIsExplainAndStatisticsOnEverySupportedEngine(t *testing.T) {
	want := []string{CmdExplain, CmdCollectStatistics}
	for _, engine := range []string{"postgres", "mysql"} {
		if got := DefaultCommands(engine); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", engine, got, want)
		}
	}
	if got := DefaultCommands("sqlserver"); len(got) != 0 {
		t.Errorf("an unknown engine gets nothing, got %v", got)
	}
}

// A collector refuses a config naming a command it does not know, so the CLI
// warns from the image tag alone. A tag that is not a version cannot be
// ordered and must stay silent, or custom images could never be installed.
func TestCommandsBeyondImage(t *testing.T) {
	both := []string{CmdExplain, CmdCollectStatistics}
	cases := []struct {
		image string
		want  []string
	}{
		{ImageRepo + ":0.4.2", []string{CmdCollectStatistics}},
		{ImageRepo + ":v0.4.2", []string{CmdCollectStatistics}},
		{ImageRepo + ":0.5.0-rc.1", []string{CmdCollectStatistics}},
		{ImageRepo + ":0.5.0", nil},
		{ImageRepo + ":0.12.1", nil},
		{ImageRepo + ":0.5.0@sha256:abc", nil},
		{ImageRepo + ":latest", nil},
		{ImageRepo + "@sha256:abc", nil},
		{"", nil},
	}
	for _, c := range cases {
		if got := CommandsBeyondImage(c.image, both); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.image, got, c.want)
		}
	}
	if got := CommandsBeyondImage(ImageRepo+":0.4.2", []string{CmdExplain}); got != nil {
		t.Errorf("explain predates every installable image, got %v", got)
	}
}
