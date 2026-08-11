package collector

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// withConfigHome redirects the per-user config tree into a temp dir by setting
// XDG_CONFIG_HOME (honored by internal/config.Dir). Fully hermetic.
func withConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	return home
}

func TestDir_createsCollectorDir(t *testing.T) {
	home := withConfigHome(t)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	want := filepath.Join(home, "dbgorilla", "collector")
	if dir != want {
		t.Errorf("Dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("collector dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected a directory at %q", dir)
	}
}

func TestPathHelpers(t *testing.T) {
	home := withConfigHome(t)
	base := filepath.Join(home, "dbgorilla", "collector")

	cp, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if cp != filepath.Join(base, configFile) {
		t.Errorf("ConfigPath = %q", cp)
	}

	ep, err := EnvPath()
	if err != nil {
		t.Fatalf("EnvPath: %v", err)
	}
	if ep != filepath.Join(base, envFile) {
		t.Errorf("EnvPath = %q", ep)
	}

	sp, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	if sp != filepath.Join(base, stateFile) {
		t.Errorf("statePath = %q", sp)
	}
}

func TestLoadState_missingReturnsNil(t *testing.T) {
	withConfigHome(t)
	s, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState on missing file: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil state when no file exists, got %+v", s)
	}
}

func TestSaveLoadState_roundTrip(t *testing.T) {
	withConfigHome(t)
	want := &State{
		AgentID:       "agent-1",
		TenantID:      "tenant-1",
		Domain:        "app.dbgorilla.com",
		ContainerName: DefaultContainerName,
		Image:         DefaultImage,
		ConfigPath:    "/x/collector.toml",
		EnvFilePath:   "/x/collector.env",
		CACertPath:    "/x/ca.pem",
		TargetName:    "orders",
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got == nil {
		t.Fatal("LoadState returned nil after SaveState")
	}
	if got.AgentID != want.AgentID || got.TenantID != want.TenantID ||
		got.Image != want.Image || got.TargetName != want.TargetName ||
		got.CACertPath != want.CACertPath || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// The state file must be owner-only (0600).
	sp, _ := statePath()
	info, err := os.Stat(sp)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

func TestSaveState_overwrites(t *testing.T) {
	withConfigHome(t)
	if err := SaveState(&State{AgentID: "first"}); err != nil {
		t.Fatalf("SaveState first: %v", err)
	}
	if err := SaveState(&State{AgentID: "second"}); err != nil {
		t.Fatalf("SaveState second: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.AgentID != "second" {
		t.Errorf("overwrite failed: AgentID = %q, want second", got.AgentID)
	}
}

func TestLoadState_corruptJSON(t *testing.T) {
	withConfigHome(t)
	sp, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	if err := os.WriteFile(sp, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := LoadState(); err == nil {
		t.Error("expected a parse error for malformed state JSON")
	}
}

func TestRemoveState(t *testing.T) {
	withConfigHome(t)
	// Removing when nothing exists is a no-op (idempotent).
	if err := RemoveState(); err != nil {
		t.Errorf("RemoveState on missing file: %v", err)
	}
	if err := SaveState(&State{AgentID: "a"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := RemoveState(); err != nil {
		t.Fatalf("RemoveState: %v", err)
	}
	if s, _ := LoadState(); s != nil {
		t.Error("state should be gone after RemoveState")
	}
}

// TestStore_dirResolutionError points XDG_CONFIG_HOME at a regular file so the
// directory can't be created, exercising the error-propagation paths that share
// the Dir() failure.
func TestStore_dirResolutionError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", f)

	if _, err := Dir(); err == nil {
		t.Error("Dir should fail when the config base is a file")
	}
	if _, err := ConfigPath(); err == nil {
		t.Error("ConfigPath should propagate the Dir error")
	}
	if _, err := EnvPath(); err == nil {
		t.Error("EnvPath should propagate the Dir error")
	}
	if _, err := LoadState(); err == nil {
		t.Error("LoadState should propagate the Dir error")
	}
	if err := SaveState(&State{}); err == nil {
		t.Error("SaveState should propagate the Dir error")
	}
	if err := RemoveState(); err == nil {
		t.Error("RemoveState should propagate the Dir error")
	}
}

func TestLoadState_readErrorNotMissing(t *testing.T) {
	withConfigHome(t)
	sp, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	// A directory where the state file should be: ReadFile fails with an error
	// that is not os.IsNotExist, so LoadState surfaces it rather than treating
	// it as "no collector installed".
	if err := os.Mkdir(sp, 0700); err != nil {
		t.Fatalf("seed dir at state path: %v", err)
	}
	if _, err := LoadState(); err == nil {
		t.Error("expected a read error when the state path is a directory")
	}
}

func TestSaveState_unwritableDir(t *testing.T) {
	withConfigHome(t)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	// Make the collector dir non-writable so the atomic tempfile write fails.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) }) // restore before TempDir cleanup

	if err := SaveState(&State{AgentID: "a"}); err == nil {
		t.Error("expected error writing state into a non-writable dir")
	}
}

func TestWriteEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.env")
	if err := WriteEnvFile(path, "s3cr3t", "pgpass"); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env-file: %v", err)
	}
	want := SecretEnv + "=s3cr3t\n" + DBPasswordEnv + "=pgpass\n"
	if string(data) != want {
		t.Errorf("env-file content = %q, want %q", data, want)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("env-file mode = %o, want 600", perm)
	}
}

func TestWriteEnvFile_error(t *testing.T) {
	// Parent directory does not exist -> write of the tempfile fails.
	path := filepath.Join(t.TempDir(), "missing", "collector.env")
	if err := WriteEnvFile(path, "s", "p"); err == nil {
		t.Error("expected error writing into a nonexistent directory")
	}
}

func TestWriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.toml")
	const contents = "[dbgorilla]\nagent_id = \"a\"\n"
	if err := WriteConfig(path, contents); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != contents {
		t.Errorf("config content = %q, want %q", data, contents)
	}
	// collector.toml is bind-mounted read-only into the container -> 0644.
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("config mode = %o, want 644", perm)
	}
}

func TestWriteConfig_error(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "collector.toml")
	if err := WriteConfig(path, "x"); err == nil {
		t.Error("expected error writing into a nonexistent directory")
	}
}

func TestSecretKeyNames(t *testing.T) {
	if k := secretKey("agent-9"); k != "collector-secret:agent-9" {
		t.Errorf("secretKey = %q", k)
	}
	if k := dbPassKey("agent-9"); k != "collector-dbpass:agent-9" {
		t.Errorf("dbPassKey = %q", k)
	}
}

func TestSecrets_roundTripAndClear(t *testing.T) {
	keyring.MockInit()
	const agent = "agent-1"

	if err := StoreSecrets(agent, "the-secret", "the-dbpass"); err != nil {
		t.Fatalf("StoreSecrets: %v", err)
	}
	secret, dbpass, err := LoadSecrets(agent)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if secret != "the-secret" || dbpass != "the-dbpass" {
		t.Errorf("LoadSecrets = (%q, %q), want (the-secret, the-dbpass)", secret, dbpass)
	}

	// Entries are keyed per agent, so a different agent must not collide.
	if _, _, err := LoadSecrets("other-agent"); err == nil {
		t.Error("expected an error loading secrets for an unknown agent")
	}

	ClearSecrets(agent)
	if _, _, err := LoadSecrets(agent); err == nil {
		t.Error("expected an error after ClearSecrets")
	}
}

// An unavailable keychain must NOT fail the install: the collector identity is
// already minted by this point, so erroring here strands it server-side.
// Secrets fall back to a 0600 file, and reading them back must find it.
func TestStoreSecrets_keychainUnavailable_fallsBackToFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit) // restore a working mock for later tests

	if err := StoreSecrets("agent-1", "s3cret", "dbpass"); err != nil {
		t.Fatalf("StoreSecrets should fall back to a file, got %v", err)
	}

	path, err := secretsFallbackPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fallback file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("fallback file mode = %o, want 600", perm)
	}

	secret, dbPass, err := LoadSecrets("agent-1")
	if err != nil {
		t.Fatalf("LoadSecrets from fallback: %v", err)
	}
	if secret != "s3cret" || dbPass != "dbpass" {
		t.Errorf("got (%q,%q), want (s3cret,dbpass)", secret, dbPass)
	}

	ClearSecrets("agent-1")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ClearSecrets should remove the fallback file")
	}
}

func TestLoadSecrets_noKeychainAndNoFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit)

	if _, _, err := LoadSecrets("agent-1"); err == nil {
		t.Fatal("expected an error when neither keychain nor fallback file has the secrets")
	}
}
