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
			// The comment explains both default commands, and says what each
			// never touches: explain never runs the query, collect_statistics
			// never copies a row.
			for _, want := range []string{"explain", "never runs your queries", "collect_statistics", "never table rows"} {
				if !strings.Contains(commandsComment, want) {
					t.Errorf("comment does not say %q:\n%s", want, commandsComment)
				}
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

// The comment must never cost a database on AWS: a config that fits keeps it,
// and one that fits only without it is sent without it.
func TestEncodeStackConfigDropsCommentsOnlyWhenNeeded(t *testing.T) {
	targets := func(n int) []AwsTarget {
		var out []AwsTarget
		for i := 0; i < n; i++ {
			id := "db-" + strings.Repeat("x", 2) + string(rune('a'+i))
			out = append(out, AwsTarget{Name: id, InstanceID: id, Host: id + ".c1a2b3c4d5e6.us-east-1.rds.amazonaws.com",
				Port: 5432, User: "dbgorilla", Commands: DefaultCommands("postgres")})
		}
		return out
	}
	small, err := awsConfigTOML("a", "t", "us-east-1", targets(1), Endpoints{}, true)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encodeStackConfig(small)
	if err != nil {
		t.Fatal(err)
	}
	if dec, _ := DecodeConfig(enc); !strings.Contains(dec, commandsComment) {
		t.Error("a config that fits should keep its comment")
	}

	// Grow until the full text no longer fits but the compact one does.
	for n := 2; n < 40; n++ {
		full, err := awsConfigTOML("a", "t", "us-east-1", targets(n), Endpoints{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := EncodeConfig(full); err == nil {
			continue
		}
		if _, err := EncodeConfig(CompactConfig(full)); err != nil {
			t.Skip("no size where only the compact form fits")
		}
		enc, err := encodeStackConfig(full)
		if err != nil {
			t.Fatalf("n=%d: should fit once comments are dropped: %v", n, err)
		}
		dec, _ := DecodeConfig(enc)
		if strings.Contains(dec, "#") {
			t.Errorf("n=%d: comments should be dropped:\n%s", n, dec)
		}
		if _, err := StrictParseConfig(dec); err != nil {
			t.Errorf("compacted config does not parse: %v", err)
		}
		return
	}
	t.Fatal("never exceeded the limit")
}
