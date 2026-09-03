package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// lowkeyTheme is huh's minimal base theme with the accent colors flattened to
// the terminal's default foreground — emphasis comes from bold, not a loud
// purple. The interactive prompts should read like the rest of the plain
// output, just with live selection, rather than looking like a separate app.
func lowkeyTheme() *huh.Theme {
	t := huh.ThemeBase()
	flatten := func(f *huh.FieldStyles) {
		f.Title = f.Title.UnsetForeground().Bold(true)
		f.SelectSelector = f.SelectSelector.UnsetForeground()
		f.MultiSelectSelector = f.MultiSelectSelector.UnsetForeground()
		f.SelectedOption = f.SelectedOption.UnsetForeground().Bold(true)
		f.SelectedPrefix = f.SelectedPrefix.UnsetForeground()
		f.UnselectedPrefix = f.UnselectedPrefix.UnsetForeground()
		f.FocusedButton = f.FocusedButton.UnsetForeground().UnsetBackground().Bold(true).Underline(true)
		f.BlurredButton = f.BlurredButton.UnsetForeground().UnsetBackground()
		f.NextIndicator = f.NextIndicator.UnsetForeground()
		f.PrevIndicator = f.PrevIndicator.UnsetForeground()
	}
	flatten(&t.Focused)
	flatten(&t.Blurred)
	return t
}

// stdinIsTerminal reports whether stdin is a real terminal. A seam because
// every "ask the user" branch in the CLI hangs off it, and a test process never
// has one -- without it those branches are unreachable, which is precisely
// backwards for the paths that decide whether to drop TLS or which databases to
// monitor.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// interactiveTerminal reports whether stdin is a real terminal (so a huh prompt
// or spinner can run). CI / piped / redirected input gets the plain path.
func interactiveTerminal() bool {
	return stdinIsTerminal()
}

// formIO, when set, redirects a form's keystrokes and rendering. Nil in
// production (the form owns the real terminal); tests set it to drive a real
// form with scripted input, so the prompts are exercised rather than stubbed
// out — the selection and validation logic inside them is the part that decides
// what gets deployed.
var formIO struct {
	in  io.Reader
	out io.Writer
	// accessible selects huh's line-based mode. Tests use it because the full
	// terminal UI drives a render loop that never terminates against a
	// non-terminal reader; the field values, validation and our own handling of
	// the result are the same either way.
	accessible bool
}

// runForm runs the given huh fields as one themed form.
func runForm(fields ...huh.Field) error {
	f := huh.NewForm(huh.NewGroup(fields...)).WithTheme(lowkeyTheme())
	if formIO.in != nil {
		f = f.WithInput(formIO.in)
	}
	if formIO.out != nil {
		f = f.WithOutput(formIO.out)
	}
	if formIO.accessible {
		f = f.WithAccessible(true)
	}
	return f.Run()
}

// withSpinner runs a slow action under a spinner when interactive; otherwise it
// just runs it (so CI/logs stay linear). The action owns capturing any output
// it wants to show on failure.
func withSpinner(title string, action func() error) error {
	if !interactiveTerminal() {
		return action()
	}
	var err error
	// Run's own error is a Ctrl-C interrupt: the action goroutine was
	// abandoned mid-flight, so its (still nil) err must never read as success
	// — callers sequence destructive steps behind this return, and an
	// uninstall interrupted here once printed "deleted" for a live deployment.
	if rerr := spinner.New().
		Title(" " + title).
		Style(lipgloss.NewStyle()). // default text color, not huh's red accent
		Action(func() { err = action() }).
		Run(); rerr != nil {
		return fmt.Errorf("interrupted — the last step may or may not have completed; check with `dbg collector status`: %w", rerr)
	}
	return err
}
