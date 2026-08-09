package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setup(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DBGORILLA_API_URL", "")
	_ = os.MkdirAll(filepath.Join(home, ".config", "dbgorilla"), 0700)
	return home
}

func writeUserConfig(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, ".config", "dbgorilla", "cli.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// --- ResolveAPIURL priority -----------------------------------------------

func TestResolveAPIURL_FlagWins(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\nurl = \"https://from-config\"\n")
	t.Setenv("DBGORILLA_API_URL", "https://from-env")

	url, src := ResolveAPIURL("https://from-flag")
	if url != "https://from-flag" || src != SourceFlag {
		t.Errorf("got (%q,%v), want (https://from-flag, flag)", url, src)
	}
}

func TestResolveAPIURL_EnvBeatsConfig(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\nurl = \"https://from-config\"\n")
	t.Setenv("DBGORILLA_API_URL", "https://from-env")

	url, src := ResolveAPIURL("")
	if url != "https://from-env" || src != SourceEnv {
		t.Errorf("got (%q,%v), want env wins", url, src)
	}
}

func TestResolveAPIURL_UserConfigUsed(t *testing.T) {
	home := setup(t)
	writeUserConfig(t, home, "[api]\nurl = \"https://from-config\"\n")

	url, src := ResolveAPIURL("")
	if url != "https://from-config" || src != SourceUser {
		t.Errorf("got (%q,%v), want user-config", url, src)
	}
}

func TestResolveAPIURL_EmptyWhenNothingSet(t *testing.T) {
	setup(t)

	url, src := ResolveAPIURL("")
	if url != "" || src != SourceNone {
		t.Errorf("got (%q,%v), want empty/none", url, src)
	}
}

// --- LoadUser / SaveUser --------------------------------------------------

func TestSaveUser_RoundTrips(t *testing.T) {
	setup(t)

	cfg := &Config{API: APIConfig{URL: "https://example"}}
	if err := cfg.SaveUser(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.API.URL != "https://example" {
		t.Errorf("round-trip: got %q", loaded.API.URL)
	}
}

func TestLoadUser_MissingFileReturnsZeroValue(t *testing.T) {
	setup(t)
	// Wipe the dir setup() created so the file truly doesn't exist.
	home, _ := os.UserHomeDir()
	os.RemoveAll(filepath.Join(home, ".config", "dbgorilla"))

	cfg, err := LoadUser()
	if err != nil {
		t.Fatalf("LoadUser missing: %v", err)
	}
	if cfg.API.URL != "" {
		t.Errorf("expected zero, got %q", cfg.API.URL)
	}
}

// --- SystemConfigPath -----------------------------------------------------

func TestSystemConfigPath_PerPlatform(t *testing.T) {
	got := SystemConfigPath()
	switch runtime.GOOS {
	case "darwin":
		if got != "/Library/Application Support/dbgorilla/cli.toml" {
			t.Errorf("darwin: got %q", got)
		}
	case "windows":
		if got != `C:\ProgramData\dbgorilla\cli.toml` {
			t.Errorf("windows: got %q", got)
		}
	default:
		if got != "/etc/dbgorilla/cli.toml" {
			t.Errorf("linux/other: got %q", got)
		}
	}
}
