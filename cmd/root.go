package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// Set at build time via ldflags. goreleaser populates these.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func init() {
	// Push the resolved version into the api package so every outgoing
	// User-Agent identifies the CLI version (cannot import cmd from api due
	// to cycles, so we inject it the other way). resolveVersion() falls back
	// to Go's embedded build info for `go install`ed binaries.
	v, _, _ := resolveVersion()
	api.SetUserAgentVersion(v)
}

var rootCmd = &cobra.Command{
	Use:   "dbgorilla",
	Short: "DBGorilla CLI -- sign in and connect your IDE",
	Long: `dbgorilla is the command-line interface for DBGorilla (aliased as "dbg").

  Quick start:
    dbgorilla login          Sign in to your DBGorilla deployment
    dbgorilla setup-ide      Configure Claude Code to use DBGorilla via MCP
    dbgorilla doctor         Verify everything is working`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Resolves color before any subcommand's RunE prints anything. Runs once
	// per real invocation; unit tests that call a command's RunE directly
	// skip this entirely, so style stays disabled (plain text) in tests
	// unless a test explicitly opts in via style.SetEnabled.
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		style.SetEnabled(resolveColor(cmd))
		return nil
	},
}

func Execute() error {
	err := rootCmd.Execute()
	if err != nil {
		// errDoctorFailed is a sentinel for `dbgorilla doctor`: the command
		// already printed per-check failures, so suppress the redundant
		// "Error: doctor checks failed" trailer. Exit non-zero via the
		// returned error so callers / shells see the bad status code.
		if !errors.Is(err, errDoctorFailed) {
			fmt.Fprintf(os.Stderr, "%s %s\n", style.Error("Error:"), err)
		}
	}
	return err
}

func init() {
	rootCmd.PersistentFlags().String("api-url", "", "DBGorilla API URL (overrides config and DBGORILLA_API_URL)")
	rootCmd.PersistentFlags().BoolP("insecure", "k", false, "Skip TLS certificate verification (dev/internal environments)")
	rootCmd.PersistentFlags().Bool("color", false, "Force color output even when stdout isn't a terminal")
	rootCmd.PersistentFlags().Bool("no-color", false, "Disable color output (also honors the NO_COLOR env var)")
}

// resolveColor returns whether output should be colored for this invocation.
// An explicit --no-color wins over an explicit --color, which wins over
// auto-detection (tty check + NO_COLOR + TERM=dumb via style.Detect).
func resolveColor(cmd *cobra.Command) bool {
	if v, _ := cmd.Flags().GetBool("no-color"); v && cmd.Flags().Changed("no-color") {
		return false
	}
	if v, _ := cmd.Flags().GetBool("color"); v && cmd.Flags().Changed("color") {
		return true
	}
	return style.Detect()
}

// newAPIClient creates an authenticated API client honouring --api-url and
// --insecure flags. The client picks up tokens from the keychain on its own.
func newAPIClient(cmd *cobra.Command) *api.Client {
	flagURL, _ := cmd.Flags().GetString("api-url")
	apiURL, _ := config.ResolveAPIURL(flagURL)
	if resolveInsecure(cmd) {
		return api.NewInsecureClient(apiURL)
	}
	return api.NewClient(apiURL)
}

// resolveInsecure returns whether TLS verification should be skipped for
// this invocation. Wraps config.ResolveInsecure with Cobra's flag-set
// semantics: an explicit --insecure (even =false) wins over persisted state.
func resolveInsecure(cmd *cobra.Command) bool {
	flagSet := cmd.Flags().Changed("insecure")
	flagVal, _ := cmd.Flags().GetBool("insecure")
	return config.ResolveInsecure(flagVal, flagSet)
}

// requireAPIURL returns the resolved API URL. Resolution can no longer come
// up empty (config.ResolveAPIURL falls back to the production default), so
// the error return exists only for signature stability at the call sites;
// a wrong-deployment mistake surfaces as a connection/auth error with the
// URL in it, not here.
func requireAPIURL(cmd *cobra.Command) (string, error) {
	flagURL, _ := cmd.Flags().GetString("api-url")
	url, _ := config.ResolveAPIURL(flagURL)
	return url, nil
}

// requireLogin returns the stored tokens or an actionable error.
func requireLogin() (*auth.Tokens, error) {
	t, _ := auth.LoadTokens()
	if t == nil {
		return nil, fmt.Errorf("not logged in. Run: dbgorilla login")
	}
	return t, nil
}
