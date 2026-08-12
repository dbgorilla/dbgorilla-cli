package collector

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// Health and RecentLogs exist to tell a crash-looping collector apart from a
// connection problem. Before them, `collector install` printed a green
// "container started" over a container dying every two seconds and then blamed
// the network.

func TestRunnerHealth(t *testing.T) {
	r := Runner{Name: "dbg-collector"}

	t.Run("running container", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_HEALTH_OUT", "running 0")
		state, restarts, err := r.Health()
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if state != "running" || restarts != 0 {
			t.Errorf("got (%q,%d), want (running,0)", state, restarts)
		}
	})

	t.Run("restart loop is visible in both fields", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_HEALTH_OUT", "restarting 7")
		state, restarts, err := r.Health()
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if state != "restarting" || restarts != 7 {
			t.Errorf("got (%q,%d), want (restarting,7)", state, restarts)
		}
	})

	t.Run("no such container is an error", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_HEALTH_EXIT", "1")
		t.Setenv("HP_INSPECT_HEALTH_STDERR", "Error: No such object: dbg-collector")
		if _, _, err := r.Health(); err == nil {
			t.Fatal("expected an error when the container does not exist")
		}
	})

	t.Run("unexpected output shape is an error, not a wrong answer", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_HEALTH_OUT", "running")
		_, _, err := r.Health()
		if err == nil {
			t.Fatal("a half-formed inspect result must not be reported as healthy")
		}
		if !strings.Contains(err.Error(), "dbg-collector") {
			t.Errorf("error should name the container, got: %v", err)
		}
	})

	// A non-numeric restart count still leaves the state usable; treating the
	// whole call as failed would lose the one fact we did get.
	t.Run("unparseable restart count keeps the state", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_HEALTH_OUT", "running many")
		state, restarts, err := r.Health()
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if state != "running" || restarts != 0 {
			t.Errorf("got (%q,%d), want (running,0)", state, restarts)
		}
	})
}

func TestRunnerRecentLogs(t *testing.T) {
	r := Runner{Name: "dbg-collector"}

	t.Run("returns the container's own output", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_GENERIC_OUT", "config error: cannot read /etc/dbgorilla/collector.toml")
		got := r.RecentLogs(15)
		if !strings.Contains(got, "cannot read") {
			t.Errorf("logs = %q", got)
		}
		// Trailing newlines would break the indented rendering in the diagnosis.
		if strings.HasSuffix(got, "\n") {
			t.Error("logs should be trimmed")
		}
	})

	// Best-effort: this runs while already reporting a failure, so a docker
	// error must degrade to "no logs" rather than replace the diagnosis.
	t.Run("docker failure yields empty, not an error", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_GENERIC_EXIT", "1")
		if got := r.RecentLogs(15); got != "" {
			t.Errorf("logs = %q, want empty on failure", got)
		}
	})
}

// --- store: the error paths that strand an install ------------------------

func TestSaveState_UnwritableDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	// Create the collector dir, then make it read-only so the atomic write fails.
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if err := SaveState(&State{AgentID: "agent-1"}); err == nil {
		t.Fatal("an unwritable state directory must be reported, not swallowed")
	}
}

func TestRemoveState_MissingFileIsNotAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Uninstall runs this on a machine that may never have installed.
	if err := RemoveState(); err != nil {
		t.Fatalf("removing a state file that was never written must succeed, got %v", err)
	}
}

func TestWriteConfig_IsContainerReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.toml")
	if err := WriteConfig(path, "[collector]\n"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 0644 on purpose: the container runs as a non-root user and must read it.
	// The file holds only ${ENV} references, never a secret.
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("mode = %o, want 644 so the container can read it", perm)
	}
}

func TestWriteConfig_UnwritablePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	err := WriteConfig(filepath.Join(dir, "collector.toml"), "[collector]\n")
	if err == nil || !strings.Contains(err.Error(), "collector.toml") {
		t.Fatalf("err = %v, want a write failure naming the file", err)
	}
}

// A keyring that accepts the first write and fails the second used to leave the
// two halves disagreeing: a stored collector secret with no database password.
func TestStoreSecrets_PartialKeyringWriteFallsBackCleanly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit)

	if err := StoreSecrets("agent-1", "s3cret", "dbpass"); err != nil {
		t.Fatalf("StoreSecrets: %v", err)
	}
	secret, dbPass, err := LoadSecrets("agent-1")
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if secret != "s3cret" || dbPass != "dbpass" {
		t.Errorf("got (%q,%q), want both halves", secret, dbPass)
	}
}

// A corrupt fallback file must say so rather than hand back empty credentials
// that fail later with a confusing authentication error.
func TestLoadSecrets_CorruptFallbackFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit)

	path, err := secretsFallbackPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = LoadSecrets("agent-1")
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want a parse failure", err)
	}
}
