package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// The interactive pickers decide what gets deployed, whether TLS is dropped,
// and which query-analysis commands a collector may run. They are driven here
// as real forms with scripted answers rather than stubbed out, so the binding
// and validation inside them is actually exercised.
//
// The forms run in huh's line-based (accessible) mode: the full terminal UI
// drives a render loop that never terminates against a non-terminal reader.
// Field values, validation and our handling of the result are the same.

// scriptForm runs fn with the forms answering from answers, and returns
// whatever the form printed.
func scriptForm(t *testing.T, answers string, fn func()) string {
	t.Helper()
	var out bytes.Buffer
	origIn, origOut, origAcc := formIO.in, formIO.out, formIO.accessible
	formIO.in, formIO.out, formIO.accessible = strings.NewReader(answers), &out, true
	t.Cleanup(func() { formIO.in, formIO.out, formIO.accessible = origIn, origOut, origAcc })
	fn()
	return out.String()
}

func TestLowkeyTheme_EmphasisIsBoldNotColor(t *testing.T) {
	// The prompts should read like the rest of the plain output rather than
	// looking like a separate app.
	theme := lowkeyTheme()
	if theme == nil {
		t.Fatal("lowkeyTheme returned nil")
	}
	if !theme.Focused.Title.GetBold() {
		t.Error("emphasis should come from bold, so the focused title must be bold")
	}
	if !theme.Blurred.Title.GetBold() {
		t.Error("the blurred theme should be flattened the same way")
	}
}

func TestPromptYesNo(t *testing.T) {
	t.Run("yes is yes", func(t *testing.T) {
		var got bool
		out := scriptForm(t, "y\n", func() { got = promptYesNo("Continue?", false) })
		if !got {
			t.Errorf("got false, want true (out=%q)", out)
		}
	})

	t.Run("no is no", func(t *testing.T) {
		var got bool
		scriptForm(t, "n\n", func() { got = promptYesNo("Continue?", true) })
		if got {
			t.Error("got true, want false")
		}
	})

	// The question text has to reach the user, or they are answering blind.
	t.Run("the question is shown", func(t *testing.T) {
		out := scriptForm(t, "y\n", func() {
			_ = promptYesNo("Connect to this remote database without TLS?", false)
		})
		if !strings.Contains(out, "without TLS") {
			t.Errorf("out = %q", out)
		}
	})

	// A closed input must fall back to the stated default, not invert it: the
	// callers use this for "connect without TLS?", where the default is no.
	t.Run("aborted input keeps the safe default", func(t *testing.T) {
		var got bool
		scriptForm(t, "", func() { got = promptYesNo("Connect without TLS?", false) })
		if got {
			t.Error("an aborted prompt must not read as consent")
		}
	})
}

// Only the empty/abort outcome is reachable in line-based mode — huh does not
// run a no-echo password field there. What matters for the callers is that an
// unanswered prompt means "no password given", i.e. keep IAM auth, rather than
// an empty string that gets treated as a real password.
func TestPromptPasswordOptional_UnansweredMeansNoPassword(t *testing.T) {
	var got string
	scriptForm(t, "", func() { got = promptPasswordOptional("DB password") })
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestPromptGrantPassword_UnansweredMeansNoPassword(t *testing.T) {
	var got string
	scriptForm(t, "", func() { got = promptGrantPassword("postgres") })
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestPromptCommands(t *testing.T) {
	// Every command is pre-selected; confirming without changing anything
	// enables the lot.
	t.Run("confirming keeps every command", func(t *testing.T) {
		var got []string
		scriptForm(t, "\n\n\n", func() { got = promptCommands(collector.AwsTarget{Name: "prod"}) })
		if len(got) != len(collector.AwsCommandCatalog()) {
			t.Errorf("commands = %v, want the full catalog", got)
		}
	})

	// A cancelled checklist must not silently turn query analysis off — that
	// would quietly reduce what the collector reports.
	t.Run("abort falls back to the catalog", func(t *testing.T) {
		var got []string
		scriptForm(t, "", func() { got = promptCommands(collector.AwsTarget{Name: "prod"}) })
		if len(got) != len(collector.AwsCommandCatalog()) {
			t.Errorf("commands = %v, want the full catalog on abort", got)
		}
	})

	t.Run("the database is named in the title", func(t *testing.T) {
		out := scriptForm(t, "", func() { _ = promptCommands(collector.AwsTarget{Name: "prod-db"}) })
		if !strings.Contains(out, "prod-db") {
			t.Errorf("out = %q", out)
		}
	})
}

func TestPickTargets(t *testing.T) {
	amb := &collector.AmbiguousTargetError{
		Instances: []string{"prod-db", "staging-db"},
		Clusters:  []string{"analytics"},
	}

	// In line-based mode each option is a number and 0 confirms.
	const confirm = "0\n"

	t.Run("the candidates are listed with their engine", func(t *testing.T) {
		out := scriptForm(t, "1\n"+confirm, func() { _, _ = pickTargets(amb) })
		for _, want := range []string{"prod-db", "staging-db", "analytics"} {
			if !strings.Contains(out, want) {
				t.Errorf("every candidate should be offered; %q missing from %q", want, out)
			}
		}
		// RDS and Aurora are addressed differently, so the label matters.
		if !strings.Contains(out, "Aurora") {
			t.Errorf("the Aurora cluster should be labelled as one, got %q", out)
		}
	})

	t.Run("one pick returns it with its provider type", func(t *testing.T) {
		var got []collector.TargetChoice
		var err error
		scriptForm(t, "1\n"+confirm, func() { got, err = pickTargets(amb) })
		if err != nil {
			t.Fatalf("pickTargets: %v", err)
		}
		if len(got) != 1 || got[0].ID != "prod-db" || got[0].ProviderType != "aws_rds" {
			t.Errorf("choices = %+v, want the RDS instance prod-db", got)
		}
	})

	// An Aurora cluster must keep its provider type through the picker: RDS and
	// Aurora are addressed differently by the deployed collector.
	t.Run("an Aurora pick keeps its provider type", func(t *testing.T) {
		var got []collector.TargetChoice
		var err error
		scriptForm(t, "3\n"+confirm, func() { got, err = pickTargets(amb) })
		if err != nil {
			t.Fatalf("pickTargets: %v", err)
		}
		if len(got) != 1 || got[0].ID != "analytics" || got[0].ProviderType != "aws_aurora" {
			t.Errorf("choices = %+v, want the Aurora cluster", got)
		}
	})
}

func TestWithSpinner_NonInteractiveJustRunsTheAction(t *testing.T) {
	// CI has no terminal: the action still runs, and its error still surfaces
	// unchanged so the caller can classify it (a deploy timeout, say).
	ran := false
	if err := withSpinner("working", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withSpinner: %v", err)
	}
	if !ran {
		t.Error("the action must run without a terminal")
	}

	sentinel := io.ErrUnexpectedEOF
	if err := withSpinner("working", func() error { return sentinel }); err != sentinel {
		t.Errorf("err = %v, want the action's error unchanged", err)
	}
}

func TestInteractiveTerminal_APipeIsNotATerminal(t *testing.T) {
	// The whole non-interactive branch set depends on this being honest about
	// piped input.
	setStdin(t, "")
	if interactiveTerminal() {
		t.Error("a pipe is not a terminal")
	}
}
