package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/spf13/cobra"
)

// Set at build time via ldflags. goreleaser populates these.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func init() {
	// Push the build-time version into the api package so every outgoing
	// User-Agent identifies the CLI version (cannot import cmd from api due
	// to cycles, so we inject it the other way).
	api.SetUserAgentVersion(Version)
}

var rootCmd = &cobra.Command{
	Use:   "dbg",
	Short: "DBGorilla CLI -- sign in and connect your IDE",
	Long: `dbg is the command-line interface for DBGorilla.

  Quick start:
    dbg login          Sign in to your DBGorilla deployment
    dbg setup-ide      Configure Claude Code to use DBGorilla via MCP
    dbg doctor         Verify everything is working`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	err := rootCmd.Execute()
	if err != nil {
		// errDoctorFailed is a sentinel for `dbg doctor`: the command
		// already printed per-check failures, so suppress the redundant
		// "Error: doctor checks failed" trailer. Exit non-zero via the
		// returned error so callers / shells see the bad status code.
		if !errors.Is(err, errDoctorFailed) {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		}
	}
	return err
}

func init() {
	rootCmd.PersistentFlags().String("api-url", "", "DBGorilla API URL (overrides config and DBGORILLA_API_URL)")
	rootCmd.PersistentFlags().BoolP("insecure", "k", false, "Skip TLS certificate verification (dev/internal environments)")
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

// requireAPIURL returns the resolved API URL or an actionable error pointing
// the user at all the ways they can configure it. Used by every command that
// needs to talk to the backend.
func requireAPIURL(cmd *cobra.Command) (string, error) {
	flagURL, _ := cmd.Flags().GetString("api-url")
	url, source := config.ResolveAPIURL(flagURL)
	if url == "" {
		return "", fmt.Errorf(
			"no DBGorilla API URL configured.\n" +
				"  Set one of:\n" +
				"    dbg config set api-url https://your-deployment\n" +
				"    export DBGORILLA_API_URL=https://your-deployment\n" +
				"    dbg --api-url https://your-deployment ...\n" +
				"\n" +
				"  Or have your IT team deploy /Library/Application Support/dbgorilla/cli.toml\n" +
				"  with `[api]\\n  url = \"https://your-deployment\"`.",
		)
	}
	_ = source // available to callers if they want to log it
	return url, nil
}

// requireLogin returns the stored tokens or an actionable error.
func requireLogin() (*auth.Tokens, error) {
	t, _ := auth.LoadTokens()
	if t == nil {
		return nil, fmt.Errorf("not logged in. Run: dbg login")
	}
	return t, nil
}
