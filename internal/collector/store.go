package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/zalando/go-keyring"
)

const (
	stateFile      = "state.json"
	envFile        = "collector.env"
	configFile     = "collector.toml"
	keyringService = "dbgorilla"
)

// Dir returns the per-user collector directory (~/.config/dbgorilla/collector),
// creating it 0700 if needed.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "collector")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create collector directory: %w", err)
	}
	return dir, nil
}

// ConfigPath / EnvPath / statePath are the on-disk artifact locations.
func ConfigPath() (string, error) { return inDir(configFile) }
func EnvPath() (string, error)    { return inDir(envFile) }
func statePath() (string, error)  { return inDir(stateFile) }

func inDir(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// State records the installed collector so status/stop/uninstall work across
// CLI invocations. It holds no secrets.
type State struct {
	AgentID       string    `json:"agent_id"`
	TenantID      string    `json:"tenant_id"`
	Domain        string    `json:"domain"`
	ContainerName string    `json:"container_name"`
	Image         string    `json:"image"`
	ConfigPath    string    `json:"config_path"`
	EnvFilePath   string    `json:"env_file_path"`
	CACertPath    string    `json:"ca_cert_path,omitempty"`
	TargetName    string    `json:"target_name"`
	CreatedAt     time.Time `json:"created_at"`
}

// LoadState reads the installed-collector record. A missing file returns
// (nil, nil) — no collector installed.
func LoadState() (*State, error) {
	path, err := statePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read collector state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("cannot parse collector state: %w", err)
	}
	return &s, nil
}

// SaveState writes the record atomically (tempfile + rename, 0600).
func SaveState(s *State) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot serialize collector state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("cannot write collector state: %w", err)
	}
	return os.Rename(tmp, path)
}

// RemoveState deletes the state file (best-effort).
func RemoveState() error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- secrets (OS keychain) -----------------------------------------------

func secretKey(agentID string) string { return "collector-secret:" + agentID }
func dbPassKey(agentID string) string { return "collector-dbpass:" + agentID }

// StoreSecrets persists the collector secret and DB password in the OS
// keychain, keyed by agent id.
func StoreSecrets(agentID, secret, dbPassword string) error {
	if err := keyring.Set(keyringService, secretKey(agentID), secret); err != nil {
		return fmt.Errorf("cannot store collector secret in keychain: %w", err)
	}
	if err := keyring.Set(keyringService, dbPassKey(agentID), dbPassword); err != nil {
		return fmt.Errorf("cannot store database password in keychain: %w", err)
	}
	return nil
}

// LoadSecrets reads the collector secret and DB password from the keychain.
func LoadSecrets(agentID string) (secret, dbPassword string, err error) {
	secret, err = keyring.Get(keyringService, secretKey(agentID))
	if err != nil {
		return "", "", fmt.Errorf("cannot read collector secret from keychain: %w", err)
	}
	dbPassword, err = keyring.Get(keyringService, dbPassKey(agentID))
	if err != nil {
		return "", "", fmt.Errorf("cannot read database password from keychain: %w", err)
	}
	return secret, dbPassword, nil
}

// ClearSecrets removes both keychain entries (best-effort).
func ClearSecrets(agentID string) {
	_ = keyring.Delete(keyringService, secretKey(agentID))
	_ = keyring.Delete(keyringService, dbPassKey(agentID))
}

// WriteEnvFile materializes the secrets into a 0600 env-file that `docker run
// --env-file` reads. Called on install and on start; the file is the only
// place plaintext secrets land on disk.
func WriteEnvFile(path, secret, dbPassword string) error {
	content := fmt.Sprintf("%s=%s\n%s=%s\n", SecretEnv, secret, DBPasswordEnv, dbPassword)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return fmt.Errorf("cannot write env-file: %w", err)
	}
	return os.Rename(tmp, path)
}

// WriteConfig writes the rendered collector.toml atomically. It is 0644 (not
// 0600) because it is bind-mounted into the collector container, which runs as
// a non-root user with a read-only rootfs and must be able to read it. The file
// holds no secrets — only ${ENV} references — so world-readable is safe.
func WriteConfig(path, contents string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(contents), 0644); err != nil {
		return fmt.Errorf("cannot write collector.toml: %w", err)
	}
	if err := os.Chmod(tmp, 0644); err != nil { // WriteFile honors umask on create
		return err
	}
	return os.Rename(tmp, path)
}
