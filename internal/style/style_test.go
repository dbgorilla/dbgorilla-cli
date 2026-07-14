package style

import (
	"os"
	"strings"
	"testing"
)

// unsetEnv clears an env var for the duration of the test and restores
// whatever was there before, even if it was unset to begin with (t.Setenv
// can only set a value, not remove one).
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func withIsTerminal(t *testing.T, v bool) {
	t.Helper()
	old := isTerminal
	isTerminal = func(int) bool { return v }
	t.Cleanup(func() { isTerminal = old })
}

func TestDetect_NoColorEnvDisablesRegardlessOfTTY(t *testing.T) {
	withIsTerminal(t, true)
	t.Setenv("NO_COLOR", "") // presence disables, even set to empty per no-color.org
	if Detect() {
		t.Error("NO_COLOR present should disable color even on a real tty")
	}
}

func TestDetect_DumbTermDisables(t *testing.T) {
	unsetEnv(t, "NO_COLOR")
	withIsTerminal(t, true)
	t.Setenv("TERM", "dumb")
	if Detect() {
		t.Error("TERM=dumb should disable color even on a real tty")
	}
}

func TestDetect_FallsBackToTTYCheck(t *testing.T) {
	unsetEnv(t, "NO_COLOR")
	t.Setenv("TERM", "xterm-256color")

	withIsTerminal(t, true)
	if !Detect() {
		t.Error("want true: tty + no NO_COLOR/dumb")
	}

	withIsTerminal(t, false)
	if Detect() {
		t.Error("want false: not a tty")
	}
}

func TestWrapHelpers(t *testing.T) {
	t.Cleanup(func() { SetEnabled(false) })

	SetEnabled(true)
	for _, tc := range []struct {
		name string
		fn   func(string) string
		code string
	}{
		{"Success", Success, green},
		{"Warn", Warn, yellow},
		{"Error", Error, red},
		{"Info", Info, cyan},
		{"Bold", Bold, bold},
	} {
		got := tc.fn("hi")
		if !strings.Contains(got, tc.code) || !strings.Contains(got, reset) || !strings.Contains(got, "hi") {
			t.Errorf("%s(\"hi\") = %q, want code+text+reset", tc.name, got)
		}
	}

	SetEnabled(false)
	for _, tc := range []struct {
		name string
		fn   func(string) string
	}{
		{"Success", Success},
		{"Warn", Warn},
		{"Error", Error},
		{"Info", Info},
		{"Bold", Bold},
	} {
		if got := tc.fn("hi"); got != "hi" {
			t.Errorf("%s(\"hi\") with color disabled = %q, want unchanged \"hi\"", tc.name, got)
		}
	}
}

func TestEnabled_ReflectsSetEnabled(t *testing.T) {
	t.Cleanup(func() { SetEnabled(false) })
	SetEnabled(true)
	if !Enabled() {
		t.Error("Enabled() should reflect SetEnabled(true)")
	}
	SetEnabled(false)
	if Enabled() {
		t.Error("Enabled() should reflect SetEnabled(false)")
	}
}
