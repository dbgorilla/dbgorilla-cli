package collector

import (
	"strings"
	"testing"
)

// Every path that writes a collector.toml renders through Config.Render, so
// each builder must come out with the [commands] comment directly above the
// [commands] header, exactly once.
func TestRenderedConfigsExplainCommands(t *testing.T) {
	render := func(t *testing.T, s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return s
	}
	cases := map[string]func(t *testing.T) string{
		"docker": func(t *testing.T) string {
			s, err := Build("a", "t", Target{Name: "n", Host: "localhost", Port: 5432, User: "u"}, Endpoints{}).Render()
			return render(t, s, err)
		},
		"helm": func(t *testing.T) string {
			s, err := BuildCNPG("a", "t", CNPGTarget{}, Endpoints{}).Render()
			return render(t, s, err)
		},
		"aws": func(t *testing.T) string {
			s, err := awsConfigTOML("a", "t", "us-east-1",
				[]AwsTarget{{Name: "n", InstanceID: "db", Host: "h", Port: 5432, User: "u"}}, Endpoints{}, true)
			return render(t, s, err)
		},
		"gcp": func(t *testing.T) string {
			s, err := GcpConfigTOML("a", "t",
				[]GcpTarget{{InstanceID: "pg", Engine: "postgres"}}, Endpoints{}, false)
			return render(t, s, err)
		},
		"instaclustr": func(t *testing.T) string {
			s, err := componentsConfigTOML("a", "t",
				[]Component{{Name: "n", Engine: "postgres"}}, Endpoints{}, false)
			return render(t, s, err)
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			out := fn(t)
			if n := strings.Count(out, commandsComment); n != 1 {
				t.Fatalf("want the commands comment once, got %d:\n%s", n, out)
			}
			if !strings.Contains(out, commandsComment+"[commands]\n") {
				t.Errorf("comment is not directly above [commands]:\n%s", out)
			}
			if !strings.Contains(out, CommandsDocsURL) {
				t.Errorf("missing docs link:\n%s", out)
			}
		})
	}
}

// The aws target parses its stored config and renders it again on every
// update. Comments are dropped on parse, so the comment must not pile up, and
// the config must still decode strictly.
func TestCommandsCommentSurvivesRoundTrip(t *testing.T) {
	first, err := Build("a", "t", Target{Name: "n", Host: "h", Port: 5432, User: "u"}, Endpoints{}).Render()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := StrictParseConfig(first)
	if err != nil {
		t.Fatalf("rendered config does not parse: %v", err)
	}
	second, err := cfg.Render()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("re-render changed the config:\n--- first\n%s\n--- second\n%s", first, second)
	}
}

func TestWithCommandsCommentAtStartOfFile(t *testing.T) {
	got := withCommandsComment("[commands]\nenabled = false\n")
	if !strings.HasPrefix(got, commandsComment+"[commands]\n") {
		t.Errorf("got:\n%s", got)
	}
}
