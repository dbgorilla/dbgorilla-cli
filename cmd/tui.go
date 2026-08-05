package cmd

import (
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

// interactiveTerminal reports whether stdin is a real terminal (so a huh prompt
// or spinner can run). CI / piped / redirected input gets the plain path.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// runForm runs the given huh fields as one themed form.
func runForm(fields ...huh.Field) error {
	return huh.NewForm(huh.NewGroup(fields...)).WithTheme(lowkeyTheme()).Run()
}

// withSpinner runs a slow action under a spinner when interactive; otherwise it
// just runs it (so CI/logs stay linear). The action owns capturing any output
// it wants to show on failure.
func withSpinner(title string, action func() error) error {
	if !interactiveTerminal() {
		return action()
	}
	var err error
	_ = spinner.New().
		Title(" " + title).
		Style(lipgloss.NewStyle()). // default text color, not huh's red accent
		Action(func() { err = action() }).
		Run()
	return err
}
