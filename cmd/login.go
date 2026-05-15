package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	loginCmd.Flags().String("mode", "", "Force auth mode: sso (Keycloak device flow) or password (internal). Auto-detect if omitted.")
	loginCmd.Flags().String("tenant", "", "Tenant slug (password mode only; prompted if omitted)")
	loginCmd.Flags().String("account", "", "Account / username (password mode only; prompted if omitted)")
	rootCmd.AddCommand(loginCmd)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Sign in to DBGorilla",
	Long: `Signs in to a DBGorilla deployment and stores tokens in the OS keychain.

Auto-detects the auth mode:
  - SSO (Keycloak device flow) if the backend exposes the device-config endpoint
  - Username/password otherwise

Override with --mode sso or --mode password.`,
	RunE: runLogin,
}

func runLogin(cmd *cobra.Command, _ []string) error {
	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}
	insecure := resolveInsecure(cmd)
	// Track whether --insecure was explicitly passed on this invocation
	// (vs. inherited from config). Only an explicit pass triggers persisting
	// it -- inherited insecure means "already in config, don't re-write."
	insecureFlagSet := cmd.Flags().Changed("insecure")
	insecureFlagVal, _ := cmd.Flags().GetBool("insecure")

	// If insecure was loaded from config (not explicitly set on the command
	// line), print a visible warning so the user doesn't forget they're
	// silently skipping TLS verification across every call.
	if insecure && !insecureFlagSet {
		fmt.Fprintln(os.Stderr,
			"warning: TLS verification disabled via persisted `insecure = true` in config.\n"+
				"         Run `dbg config unset insecure` to turn off, or pass --insecure=false to override.")
	}

	// Honor Ctrl-C through the device-flow polling loop.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mode, _ := cmd.Flags().GetString("mode")
	if mode == "" {
		// Auto-detect: device-config endpoint exists → SSO; else password.
		if auth.IsDeviceFlowAvailable(ctx, apiURL, insecure) {
			mode = "sso"
		} else {
			mode = "password"
		}
	}

	switch mode {
	case "sso":
		fmt.Println("Signing in via SSO (Keycloak device flow)...")
		if _, err := auth.LoginDevice(ctx, apiURL, insecure); err != nil {
			return err
		}
	case "password":
		tenant, _ := cmd.Flags().GetString("tenant")
		account, _ := cmd.Flags().GetString("account")
		creds, err := auth.PromptCredentials(auth.PasswordCredentials{Tenant: tenant, Account: account})
		if err != nil {
			return err
		}
		if _, err := auth.LoginPassword(apiURL, insecure, creds); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown mode %q (expected sso or password)", mode)
	}

	// Print identity on success. Best-effort: if /auth/user fails for any
	// reason we still consider login a success (tokens did store).
	client := newAPIClient(cmd)
	body, status, err := client.Get("/api/v0_1/auth/user")
	if err != nil || status != http.StatusOK {
		fmt.Fprintln(os.Stderr, "Signed in (could not fetch identity).")
		return nil
	}
	var u api.UserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		fmt.Fprintln(os.Stderr, "Signed in.")
		return nil
	}
	fmt.Printf("✓ Signed in as %s  (org: %s)\n", firstNonEmpty(u.Email, u.Username), firstNonEmpty(u.Tenant, u.TenantID))

	// Persist URL + insecure so subsequent commands don't need the flags.
	// This is the DevUX fix for "I logged in but still have to specify
	// --api-url every time." Only writes when something actually changes.
	persistLoginState(apiURL, insecureFlagSet, insecureFlagVal)
	return nil
}

// persistLoginState saves the resolved api-url (and, if --insecure was
// explicitly passed, the insecure flag) into the user config. Best-effort:
// failures print a warning but do not fail login.
func persistLoginState(apiURL string, insecureFlagSet, insecureFlagVal bool) {
	cfg, _ := config.LoadUser()
	changed := false

	if cfg.API.URL != apiURL {
		cfg.API.URL = apiURL
		changed = true
	}
	// Only persist insecure when the flag was explicitly set on this
	// invocation. --insecure=true -> persist true; --insecure=false ->
	// persist false (turns off any prior insecure state).
	if insecureFlagSet && cfg.API.Insecure != insecureFlagVal {
		cfg.API.Insecure = insecureFlagVal
		changed = true
	}

	if !changed {
		return
	}
	if err := cfg.SaveUser(); err != nil {
		fmt.Fprintf(os.Stderr, "Note: signed in, but could not save config: %v\n", err)
		return
	}
	path, _ := config.UserConfigPath()
	// One-line confirmation -- enough to make "I just stopped having to pass
	// --api-url" obvious without becoming chatty. The TLS-specific guidance
	// lives in `dbg setup-ide` which is where Claude Code actually
	// connects to the MCP server.
	if insecureFlagSet && insecureFlagVal {
		fmt.Printf("  Saved api-url and insecure=true to %s\n", path)
	} else {
		fmt.Printf("  Saved api-url to %s\n", path)
	}
}

// firstNonEmpty returns the first non-empty string. Used by login + whoami
// to fall back from email to username when one is missing.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
