package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/httpx"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	loginCmd.Flags().String("mode", "", "Force auth mode: sso (Keycloak device flow) or password (internal). Auto-detect if omitted.")
	loginCmd.Flags().String("tenant", "", "Tenant slug (password mode only; prompted if omitted)")
	loginCmd.Flags().String("account", "", "Account / username (password mode only; prompted if omitted)")
	loginCmd.Flags().BoolP("verbose", "v", false, "Also show the role and the internal user/organization ids on success")
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
		fmt.Fprintln(os.Stderr, style.Warn(
			"warning: TLS verification disabled via persisted `insecure = true` in config.\n"+
				"         Run `dbgorilla config unset insecure` to turn off, or pass --insecure=false to override."))
	}

	// Honor Ctrl-C through the device-flow polling loop.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mode, _ := cmd.Flags().GetString("mode")
	// Auto-detect keeps the config it discovered. Probing and then discarding
	// it meant the device flow re-fetched and re-validated the same endpoints,
	// so every endpoint warning printed twice on the first command a new user
	// runs -- alarm fatigue on exactly the warning that should be read.
	var deviceCfg *auth.DeviceConfig
	if mode == "" {
		cfg, err := auth.DiscoverDeviceConfig(ctx, apiURL, insecure)
		switch {
		case err == nil:
			deviceCfg, mode = cfg, "sso"
		case reachedTheAPI(err):
			// The deployment answered and has no SSO. Password mode is right.
			mode = "password"
		default:
			// We never reached the API -- a stale URL redirecting elsewhere, or
			// something that is not the API answering. Falling through to a
			// password prompt would ask for credentials on behalf of a
			// deployment we could not find, and would bury the one error that
			// says how to fix it.
			return err
		}
	}

	switch mode {
	case "sso":
		fmt.Println(style.Info("Signing in via SSO (Keycloak device flow)..."))
		var err error
		if deviceCfg == nil {
			// --mode sso was forced, so nothing has been discovered yet.
			_, err = auth.LoginDevice(ctx, apiURL, insecure)
		} else {
			_, err = auth.LoginDeviceWithConfig(ctx, deviceCfg, insecure)
		}
		if err != nil {
			return err
		}
	case "password":
		tenant, _ := cmd.Flags().GetString("tenant")
		account, _ := cmd.Flags().GetString("account")
		creds, err := auth.PromptCredentials(ctx, auth.PasswordCredentials{Tenant: tenant, Account: account})
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
	fmt.Printf("%s\n", style.Success("✓ Signed in as "+describeIdentity(u)))
	if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
		printIdentityDetail(cmd.OutOrStdout(), u, "  ")
	}

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
	// lives in `dbgorilla setup-ide` which is where Claude Code actually
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

// reachedTheAPI reports whether a failed device-config lookup still tells us
// the configured deployment answered as itself. A 404 means "this deployment
// has no SSO", and dropping to password mode is correct. A refused cross-host
// redirect, a web page where JSON belongs, or a connection that never landed
// mean the API was never reached, and the user needs to see why rather than be
// handed a password prompt on behalf of a deployment we could not find.
func reachedTheAPI(err error) bool {
	var crossHost *httpx.CrossHostRedirectError
	if errors.As(err, &crossHost) {
		return false
	}
	return !errors.Is(err, auth.ErrAPIUnreachable)
}
