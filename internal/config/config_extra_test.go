package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setGOOS overrides the OS-detection seam for the duration of a test and
// restores it afterward, letting the platform-specific path branches be
// exercised from a single host.
func setGOOS(t *testing.T, v string) {
	t.Helper()
	old := goos
	goos = v
	t.Cleanup(func() { goos = old })
}

// useSystemConfig points LoadSystem at a temp file (writing body when non-empty)
// and restores the real path afterward. Returns the temp path.
func useSystemConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "system-cli.toml")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := systemConfigPathFn
	systemConfigPathFn = func() string { return path }
	t.Cleanup(func() { systemConfigPathFn = old })
	return path
}

// --- path resolution: userConfigBase / Dir --------------------------------

func TestUserConfigBase_XDGWins(t *testing.T) {
	setup(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "dbgorilla"); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
}

func TestUserConfigBase_HomeFallback(t *testing.T) {
	home := setup(t) // XDG empty, HOME set, non-windows default GOOS

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "dbgorilla"); dir != want {
		t.Errorf("Dir() = %q, want XDG default under HOME %q", dir, want)
	}
}

func TestUserConfigBase_WindowsBranch(t *testing.T) {
	setup(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	setGOOS(t, "windows")

	base, err := userConfigBase()
	if err != nil {
		t.Fatalf("windows base: %v", err)
	}
	if base == "" {
		t.Error("expected a non-empty windows config base")
	}
}

func TestDir_UserConfigBaseError(t *testing.T) {
	setup(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "") // os.UserHomeDir errors when HOME is empty (unix)
	setGOOS(t, "linux")  // force the HOME path, not the windows branch

	if _, err := Dir(); err == nil {
		t.Error("expected Dir() to error when the home directory is undeterminable")
	}
}

func TestDir_MkdirAllError(t *testing.T) {
	setup(t)
	// Point XDG at a regular FILE; MkdirAll(<file>/dbgorilla) then fails.
	fileAsBase := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsBase, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", fileAsBase)

	if _, err := Dir(); err == nil {
		t.Error("expected Dir() to error when the config dir cannot be created")
	}
}

// --- UserConfigPath / LoadUser / SaveUser error propagation ---------------

func TestUserConfigPath_Error(t *testing.T) {
	setup(t)
	t.Setenv("HOME", "")
	setGOOS(t, "linux")

	if _, err := UserConfigPath(); err == nil {
		t.Error("expected UserConfigPath() to propagate the base error")
	}
}

func TestLoadUser_Error(t *testing.T) {
	setup(t)
	t.Setenv("HOME", "")
	setGOOS(t, "linux")

	if _, err := LoadUser(); err == nil {
		t.Error("expected LoadUser() to propagate the path-resolution error")
	}
}

func TestSaveUser_Error(t *testing.T) {
	setup(t)
	t.Setenv("HOME", "")
	setGOOS(t, "linux")

	if err := (&Config{API: APIConfig{URL: "https://x"}}).SaveUser(); err == nil {
		t.Error("expected SaveUser() to propagate the path-resolution error")
	}
}

// --- loadFile error branches ----------------------------------------------

func TestLoadUser_MalformedTOML(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "this is = not valid = toml [[[\n")

	if _, err := LoadUser(); err == nil {
		t.Error("expected a parse error for malformed TOML")
	}
}

func TestLoadUser_ReadError(t *testing.T) {
	home := setup(t)
	// Make the config path a directory so ReadFile fails with a non-IsNotExist
	// error, exercising the "cannot read" branch (distinct from missing-file).
	path := filepath.Join(home, ".config", "dbgorilla", "cli.toml")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(); err == nil {
		t.Error("expected a read error when the config path is a directory")
	}
}

// --- SaveUser: tempfile open failure + atomic round-trips -----------------

func TestSaveUser_TempfileOpenError(t *testing.T) {
	home := setup(t)
	// Pre-create a directory where the tempfile would go so OpenFile fails.
	tmp := filepath.Join(home, ".config", "dbgorilla", "cli.toml.tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{API: APIConfig{URL: "https://x"}}).SaveUser(); err == nil {
		t.Error("expected SaveUser() to fail when the tempfile cannot be opened")
	}
}

func TestSaveUser_FilePermissionsAre0600(t *testing.T) {
	setup(t)
	if err := (&Config{API: APIConfig{URL: "https://x"}}).SaveUser(); err != nil {
		t.Fatal(err)
	}
	path, err := UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config perms = %o, want 0600", perm)
	}
}

func TestSaveUser_ReloadMutateResaveCycle(t *testing.T) {
	setup(t)

	// Save initial state.
	if err := (&Config{API: APIConfig{URL: "https://one", Insecure: true}}).SaveUser(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if got.API.URL != "https://one" || !got.API.Insecure {
		t.Fatalf("first reload = %+v", got.API)
	}

	// Mutate the URL, keep insecure, re-save over the existing file.
	got.API.URL = "https://two"
	if err := got.SaveUser(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if again.API.URL != "https://two" || !again.API.Insecure {
		t.Errorf("after mutate/resave = %+v, want url=https://two insecure=true", again.API)
	}
}

// --- defaults / partial config / omitempty --------------------------------

func TestLoadUser_ZeroConfigDefaults(t *testing.T) {
	setup(t)
	// A fresh (unwritten) config resolves to zero values, not garbage.
	got, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if got.API.URL != "" || got.API.Insecure {
		t.Errorf("defaults = %+v, want empty URL and insecure=false", got.API)
	}
}

func TestLoadUser_PartialConfig(t *testing.T) {
	home := setup(t)
	// Only insecure is present; URL should default to empty.
	writeUserConfig(t, home, "[api]\ninsecure = true\n")

	got, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if got.API.URL != "" {
		t.Errorf("URL = %q, want empty for a partial config", got.API.URL)
	}
	if !got.API.Insecure {
		t.Error("insecure should be true from a partial config")
	}
}

func TestSaveUser_OmitemptyForZeroConfig(t *testing.T) {
	setup(t)
	if err := (&Config{}).SaveUser(); err != nil {
		t.Fatal(err)
	}
	path, err := UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); strings.Contains(s, "url") || strings.Contains(s, "insecure") {
		t.Errorf("zero config should omit empty fields, got:\n%s", s)
	}
}

// --- ResolveAPIURL: system-config layer -----------------------------------

func TestResolveAPIURL_SystemConfigUsed(t *testing.T) {
	setup(t) // no user config file, no env
	useSystemConfig(t, "[api]\nurl = \"https://from-system\"\n")

	url, src := ResolveAPIURL("")
	if url != "https://from-system" || src != SourceSystem {
		t.Errorf("got (%q, %v), want system-config", url, src)
	}
}

func TestResolveAPIURL_UserBeatsSystem(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\nurl = \"https://from-user\"\n")
	useSystemConfig(t, "[api]\nurl = \"https://from-system\"\n")

	url, src := ResolveAPIURL("")
	if url != "https://from-user" || src != SourceUser {
		t.Errorf("got (%q, %v), want user-config to win over system", url, src)
	}
}

func TestLoadSystem_ReadsContent(t *testing.T) {
	setup(t)
	useSystemConfig(t, "[api]\nurl = \"https://sys\"\ninsecure = true\n")

	got, err := LoadSystem()
	if err != nil {
		t.Fatal(err)
	}
	if got.API.URL != "https://sys" || !got.API.Insecure {
		t.Errorf("LoadSystem = %+v", got.API)
	}
}

func TestLoadSystem_MissingReturnsZero(t *testing.T) {
	setup(t)
	useSystemConfig(t, "") // path exists but no file written

	got, err := LoadSystem()
	if err != nil {
		t.Fatalf("missing system config should not error: %v", err)
	}
	if got.API.URL != "" {
		t.Errorf("expected zero config, got %+v", got.API)
	}
}

// --- ResolveInsecure ------------------------------------------------------

func TestResolveInsecure_FlagSetTrueWins(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\ninsecure = false\n")

	if !ResolveInsecure(true, true) {
		t.Error("explicit --insecure=true should win")
	}
}

func TestResolveInsecure_FlagSetFalseOverridesPersisted(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\ninsecure = true\n")

	// --insecure=false explicitly set must override a persisted true.
	if ResolveInsecure(false, true) {
		t.Error("explicit --insecure=false should override persisted true")
	}
}

func TestResolveInsecure_UserConfig(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\ninsecure = true\n")

	if !ResolveInsecure(false, false) {
		t.Error("user-config insecure=true should apply when the flag is unset")
	}
}

func TestResolveInsecure_SystemConfig(t *testing.T) {
	setup(t) // no user insecure
	useSystemConfig(t, "[api]\ninsecure = true\n")

	if !ResolveInsecure(false, false) {
		t.Error("system-config insecure=true should apply when nothing else is set")
	}
}

func TestResolveInsecure_DefaultFalse(t *testing.T) {
	setup(t)
	useSystemConfig(t, "") // no system config either

	if ResolveInsecure(false, false) {
		t.Error("insecure should default to false when nothing is configured")
	}
}

// --- SystemConfigPath: platform branches ----------------------------------

func TestSystemConfigPath_WindowsBranch(t *testing.T) {
	setGOOS(t, "windows")
	if got := SystemConfigPath(); got != `C:\ProgramData\dbgorilla\cli.toml` {
		t.Errorf("windows system path = %q", got)
	}
}

func TestSystemConfigPath_LinuxDefaultBranch(t *testing.T) {
	setGOOS(t, "linux")
	if got := SystemConfigPath(); got != "/etc/dbgorilla/cli.toml" {
		t.Errorf("linux system path = %q", got)
	}
}
