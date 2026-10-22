package cmd

import (
	"fmt"
	"net/http"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	logoutCmd.Flags().Bool("revoke-key", false,
		"Also revoke the MCP API key on the deployment. Every editor on every machine that uses it stops until `dbgorilla setup-ide` is re-run there")
	rootCmd.AddCommand(logoutCmd)
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear the stored login on this machine",
	Long: `Signs out on this machine.

Clears the stored login tokens. The MCP API key is left alone: there is one
key per user and every editor on every machine shares it, so revoking it
here would stop all of them. Editors configured on this machine keep working.

To revoke the key as well, pass --revoke-key, or run
` + "`dbgorilla setup-ide --rotate-key`" + ` to replace it with a new one.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if revoke, _ := cmd.Flags().GetBool("revoke-key"); revoke {
			revokeMCPKey(cmd)
		}
		if err := auth.ClearTokens(); err != nil {
			return fmt.Errorf("failed to clear credentials: %w", err)
		}
		fmt.Println(style.Success("Signed out."))
		return nil
	},
}

// revokeMCPKey deletes this user's MCP API key from the deployment.
//
// Only on request. The key is shared by every editor on every machine the
// user has set up, and it never expires, so revoking it is a deliberate act
// with a blast radius well beyond this machine. Signing out is local;
// --revoke-key, `setup-ide --rotate-key` and the profile page are the ways
// to kill the key.
//
// Best effort by design. Signing out has to work when the deployment is
// unreachable or the session has already lapsed, so a failure is named and
// stepped over rather than allowed to block the local credential wipe -- the
// alternative is a user who cannot sign out of a deployment that is down.
func revokeMCPKey(cmd *cobra.Command) {
	if _, err := requireLogin(); err != nil {
		warnKeyNotRevoked("not signed in, so there is no session to revoke it with")
		return
	}
	_, status, err := newAPIClient(cmd).Delete(mcpKeyPath)
	switch {
	case err != nil:
		warnKeyNotRevoked(fmt.Sprintf("%v", err))
	case status == http.StatusOK, status == http.StatusNoContent, status == http.StatusNotFound:
		fmt.Println(style.Success("✓ MCP API key revoked. Re-run `dbgorilla setup-ide` on each machine that used it."))
	default:
		warnKeyNotRevoked(fmt.Sprintf("the deployment answered HTTP %d", status))
	}
}

func warnKeyNotRevoked(reason string) {
	fmt.Println(style.Warn("⚠  Could not revoke the MCP API key: " + reason))
	fmt.Println(style.Warn("   It is still valid. Sign in again and re-run `dbgorilla logout --revoke-key`,"))
	fmt.Println(style.Warn("   or run `dbgorilla setup-ide --rotate-key` to replace it."))
}
