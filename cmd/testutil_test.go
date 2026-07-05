package cmd

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

// isolate makes a test hermetic: an in-memory keychain, temp config/home dirs,
// no ambient API URL, and an empty PATH so no real docker/brew/claude binary
// can ever be executed. Returns the config/home root for path assertions.
func isolate(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("DBGORILLA_API_URL", "")
	// Empty PATH: any accidental real-binary lookup fails fast + deterministically.
	t.Setenv("PATH", t.TempDir())
	return home
}

// capture redirects os.Stdout+os.Stderr to a pipe for the duration of fn and
// returns everything written. A drain goroutine prevents pipe-buffer deadlock.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = w, w
	outC := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outC <- buf.String()
	}()
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	fn()
	_ = w.Close()
	os.Stdout, os.Stderr = origOut, origErr
	s := <-outC
	_ = r.Close()
	return s
}

// setStdin feeds content to os.Stdin (as a pipe, which EOFs after content).
func setStdin(t *testing.T, content string) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = io.WriteString(w, content)
		_ = w.Close()
	}()
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// writeTokens stores a non-expired access token in the (mocked) keychain so
// requireLogin / authenticated API calls succeed.
func writeTokens(t *testing.T) {
	t.Helper()
	if err := auth.StoreTokens(&auth.Tokens{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("store tokens: %v", err)
	}
}

// baseCmd builds a throwaway command carrying just the two persistent flags
// (--api-url / --insecure) every command inherits from the real root.
func baseCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("api-url", "", "")
	c.Flags().Bool("insecure", false, "")
	return c
}

// mustSet sets a flag and marks it changed (mirrors an explicit CLI pass).
func mustSet(t *testing.T, c *cobra.Command, name, val string) {
	t.Helper()
	if err := c.Flags().Set(name, val); err != nil {
		t.Fatalf("set --%s=%s: %v", name, val, err)
	}
}

// --- external-command / detection seam stubs ------------------------------

// stubExec replaces execCommand so `claude`/`brew` invocations run a fake
// subprocess (via the TestHelperProcess pattern) with scripted stdout+exit.
func stubExec(t *testing.T, stdout string, exit int) {
	t.Helper()
	orig := execCommand
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
		c.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_STDOUT="+stdout,
			"HELPER_EXIT="+strconv.Itoa(exit),
		)
		return c
	}
	t.Cleanup(func() { execCommand = orig })
}

// stubLookPath makes lookPath report a binary as present (found) or absent.
func stubLookPath(t *testing.T, found bool) {
	t.Helper()
	orig := lookPath
	lookPath = func(f string) (string, error) {
		if found {
			return "/fake/bin/" + f, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = orig })
}

// stubDetect makes detectInstalled return a fixed adapter set.
func stubDetect(t *testing.T, adapters ...ide.Adapter) {
	t.Helper()
	orig := detectInstalled
	detectInstalled = func() []ide.Adapter { return adapters }
	t.Cleanup(func() { detectInstalled = orig })
}

// stubExecutable makes osExecutable return a fixed path (Homebrew-prefix tests).
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = orig })
}

// TestHelperProcess is the child process spawned by stubExec. It is inert
// unless invoked with GO_WANT_HELPER_PROCESS=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	_, _ = io.WriteString(os.Stdout, os.Getenv("HELPER_STDOUT"))
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

// --- fake IDE adapters ----------------------------------------------------

// fakeWriter is a test double for ide.Writer whose config path (and errors)
// are fully controllable, so config-file behavior can be exercised on a
// temp file without any real IDE installed.
type fakeWriter struct {
	name, slug, topKey, path string
	pathErr                  error
}

func (f fakeWriter) Name() string { return f.name }
func (f fakeWriter) Slug() string { return f.slug }
func (f fakeWriter) Detect() bool { return true }
func (f fakeWriter) SupportedScopes() []ide.Scope {
	return []ide.Scope{ide.ScopeUser, ide.ScopeProject}
}
func (f fakeWriter) DefaultScope() ide.Scope { return ide.ScopeUser }
func (f fakeWriter) TopLevelKey() string {
	if f.topKey == "" {
		return "mcpServers"
	}
	return f.topKey
}
func (f fakeWriter) ConfigPath(ide.Scope) (string, error) {
	if f.pathErr != nil {
		return "", f.pathErr
	}
	return f.path, nil
}
func (f fakeWriter) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"url":     mcpURL,
		"headers": map[string]string{"Authorization": "Bearer " + apiKey},
	}
}

// fakeHinter is a detect-only adapter (implements Hinter, not Writer).
type fakeHinter struct{ name, slug string }

func (f fakeHinter) Name() string              { return f.name }
func (f fakeHinter) Slug() string              { return f.slug }
func (f fakeHinter) Detect() bool              { return true }
func (f fakeHinter) Hint(mcpURL string) string { return "MANUAL SETUP: " + mcpURL }

// fakeBare implements only ide.Adapter (neither Writer nor Hinter) to exercise
// the defensive "no setup path implemented" branch.
type fakeBare struct{ name, slug string }

func (f fakeBare) Name() string { return f.name }
func (f fakeBare) Slug() string { return f.slug }
func (f fakeBare) Detect() bool { return true }
