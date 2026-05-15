// Package config persists non-secret CLI configuration and resolves the API
// URL via a deterministic layered priority chain.
//
// Resolution order for the API URL (highest priority first):
//
//  1. --api-url flag
//  2. DBGORILLA_API_URL env var
//  3. $XDG_CONFIG_HOME/dbgorilla/cli.toml (per-user; defaults to
//     ~/.config/dbgorilla/cli.toml when XDG_CONFIG_HOME is unset)
//  4. /etc/dbgorilla/cli.toml        (system-wide; macOS uses
//     /Library/Application Support/dbgorilla/cli.toml)
//  5. (no default; caller surfaces a helpful error pointing at `dbg config`)
//
// The user-config location follows the XDG Base Directory spec rather than a
// proprietary dotfile dir. Dotfile-walking credential stealers find both
// anyway, and XDG alignment lets backup/sync tooling treat dbg like every
// other modern CLI.
//
// On-prem deployments don't share a single canonical URL, so we deliberately
// do not bake one in. The install script (scripts/install.sh.tmpl) writes
// the user-level config file at install time so the dev never has to type
// the URL.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

const (
	configSubdir = "dbgorilla"
	configFile   = "cli.toml"
)

// Config is the persisted CLI configuration.
type Config struct {
	API APIConfig `toml:"api"`
}

type APIConfig struct {
	URL string `toml:"url,omitempty"`
	// Insecure mirrors the --insecure flag. Persisted on login when the
	// flag was used so the user doesn't have to pass it on every call
	// against an on-prem deployment with a private CA.
	Insecure bool `toml:"insecure,omitempty"`
}

// Dir returns the per-user config directory (XDG-aware), creating it if
// needed. On Unix-like systems this is $XDG_CONFIG_HOME/dbgorilla, defaulting
// to ~/.config/dbgorilla. On Windows it's whatever os.UserConfigDir returns
// (typically %AppData%\dbgorilla).
func Dir() (string, error) {
	base, err := userConfigBase()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, configSubdir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create config directory: %w", err)
	}
	return dir, nil
}

// userConfigBase returns the base XDG-style config directory. We don't use
// os.UserConfigDir on macOS because it returns ~/Library/Application Support
// -- we want cross-platform consistency with ~/.config on macOS too, since
// dbg is a developer-facing CLI and devs expect XDG semantics regardless
// of OS.
func userConfigBase() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg, nil
	}
	if runtime.GOOS == "windows" {
		return os.UserConfigDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// UserConfigPath returns the per-user config file path.
func UserConfigPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFile), nil
}

// SystemConfigPath returns the OS-appropriate system-wide config path used by
// MDM-deployed configurations. This file is read-only from the CLI's point of
// view; we never write it.
func SystemConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/dbgorilla/cli.toml"
	case "windows":
		return `C:\ProgramData\dbgorilla\cli.toml`
	default:
		return "/etc/dbgorilla/cli.toml"
	}
}

// LoadUser reads the per-user config. A missing file returns a zero-value
// Config and nil error.
func LoadUser() (*Config, error) {
	path, err := UserConfigPath()
	if err != nil {
		return &Config{}, err
	}
	return loadFile(path)
}

// LoadSystem reads the system-wide config. A missing file returns a zero-value
// Config and nil error -- system config is optional.
func LoadSystem() (*Config, error) {
	return loadFile(SystemConfigPath())
}

func loadFile(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("cannot read config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return cfg, fmt.Errorf("cannot parse config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveUser writes the per-user config atomically. The write goes to a
// tempfile + rename, with the tempfile created via OpenFile so the mode
// (0600) is enforced even if the destination already exists with weaker
// permissions (which os.WriteFile would otherwise preserve).
func (c *Config) SaveUser() error {
	path, err := UserConfigPath()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return fmt.Errorf("cannot serialize config: %w", err)
	}
	data := buf.Bytes()
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("cannot open config tempfile: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("cannot write config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ResolvedSource describes where the resolved API URL came from. Useful for
// `dbg config get api-url` so users can see exactly which layer won.
type ResolvedSource string

const (
	SourceFlag   ResolvedSource = "flag"
	SourceEnv    ResolvedSource = "env"
	SourceUser   ResolvedSource = "user-config"
	SourceSystem ResolvedSource = "system-config"
	SourceNone   ResolvedSource = "none"
)

// ResolveAPIURL returns the API URL using the documented layered priority.
// Returns the URL plus the source it came from. Empty URL + SourceNone means
// nothing is configured and the caller should error with a helpful message.
func ResolveAPIURL(flagValue string) (string, ResolvedSource) {
	if flagValue != "" {
		return flagValue, SourceFlag
	}
	if env := os.Getenv("DBGORILLA_API_URL"); env != "" {
		return env, SourceEnv
	}
	if u, _ := LoadUser(); u != nil && u.API.URL != "" {
		return u.API.URL, SourceUser
	}
	if s, _ := LoadSystem(); s != nil && s.API.URL != "" {
		return s.API.URL, SourceSystem
	}
	return "", SourceNone
}

// ResolveInsecure returns whether TLS verification should be skipped.
//
// If the --insecure flag was explicitly set on the command line, that wins
// (including --insecure=false to deliberately override a persisted true).
// Otherwise falls back to user-config api.insecure, then system-config.
// flagSet should come from cmd.Flags().Changed("insecure").
func ResolveInsecure(flagValue, flagSet bool) bool {
	if flagSet {
		return flagValue
	}
	if u, _ := LoadUser(); u != nil && u.API.Insecure {
		return true
	}
	if s, _ := LoadSystem(); s != nil && s.API.Insecure {
		return true
	}
	return false
}
