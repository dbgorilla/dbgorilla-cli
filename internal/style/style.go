// Package style provides a handful of ANSI color helpers that stay silent
// (return the input unchanged) unless the CLI is confident the output is
// going to a terminal that renders color. Never emit raw escape codes into
// a pipe, a log file, or a terminal that doesn't understand them.
//
// Enabled defaults to false and is only ever flipped on by an explicit
// SetEnabled call -- normally once, at startup, after resolving --color/
// --no-color against Detect(). This keeps every other call site (including
// every existing test that invokes a command's RunE directly, bypassing
// cmd.Execute()'s startup hook) plain-text by default with zero risk of
// color codes leaking into output nobody asked to colorize.
package style

import (
	"os"

	"golang.org/x/term"
)

// isTerminal indirects term.IsTerminal so tests can fake tty-ness without a
// real pty.
var isTerminal = term.IsTerminal

var enabled bool

// SetEnabled sets whether the wrap helpers below emit color escapes.
func SetEnabled(v bool) { enabled = v }

// Enabled reports the current setting.
func Enabled() bool { return enabled }

// Detect reports whether stdout looks like a terminal that renders ANSI
// color, honoring the NO_COLOR convention (https://no-color.org -- presence
// of the variable disables color regardless of its value) and TERM=dumb.
// It does not itself enable anything; callers combine it with any --color/
// --no-color override and pass the result to SetEnabled.
func Detect() bool {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(int(os.Stdout.Fd()))
}

const (
	reset  = "\x1b[0m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[96m"
	bold   = "\x1b[1m"
)

func wrap(code, s string) string {
	if !enabled {
		return s
	}
	return code + s + reset
}

// Success wraps text destined for a positive/completed status (green).
func Success(s string) string { return wrap(green, s) }

// Warn wraps text destined for a non-fatal caution (yellow).
func Warn(s string) string { return wrap(yellow, s) }

// Error wraps text destined for a failure (red).
func Error(s string) string { return wrap(red, s) }

// Info wraps text that deserves emphasis without implying success, failure,
// or caution -- section headers, "this is in progress" verbs, and explicit
// dry-run/preview markers (cyan).
func Info(s string) string { return wrap(cyan, s) }

// Bold wraps text that should stand out without implying success/failure.
func Bold(s string) string { return wrap(bold, s) }
