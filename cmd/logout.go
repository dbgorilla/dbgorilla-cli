package cmd

import (
	"fmt"
	"net/http"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(logoutCmd)
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear stored credentials and revoke the MCP API key",
	Long: `Signs out of the deployment.

Clears the stored login tokens and revokes the MCP API key, so nothing left
on this machine can still reach the deployment. The MCP entries in your
editors are left in place but stop working; ` + "`dbgorilla setup-ide --remove`" + `
takes those out.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		revokeMCPKey(cmd)
		if err := auth.ClearTokens(); err != nil {
			return fmt.Errorf("failed to clear credentials: %w", err)
		}
		fmt.Println(style.Success("Signed out."))
		return nil
	},
}

// revokeMCPKey deletes this user's MCP API key from the deployment.
//
// Clearing the login tokens on its own left the MCP key live, and it is the
// credential that actually reaches the deployment: an editor configured with
// it kept working after sign-out, and the key never expires, so "signed out"
// meant nothing on the machine that mattered.
//
// Best effort by design. Signing out has to work when the deployment is
// unreachable or the session has already lapsed, so a failure is named and
// stepped over rather than allowed to block the local credential wipe -- the
// alternative is a user who cannot sign out of a deployment that is down.
func revokeMCPKey(cmd *cobra.Command) {
	if _, err := requireLogin(); err != nil {
		return // no session to revoke anything with
	}
	if _, err := requireAPIURL(cmd); err != nil {
		return
	}
	_, status, err := newAPIClient(cmd).Delete(mcpKeyPath)
	switch {
	case err != nil:
		warnKeyNotRevoked(fmt.Sprintf("%v", err))
	case status == http.StatusOK, status == http.StatusNoContent, status == http.StatusNotFound:
		// Revoked, or there was nothing to revoke.
	default:
		warnKeyNotRevoked(fmt.Sprintf("the deployment answered HTTP %d", status))
	}
}

func warnKeyNotRevoked(reason string) {
	fmt.Println(style.Warn("⚠  Could not revoke the MCP API key: " + reason))
	fmt.Println(style.Warn("   It is still valid. Sign in again and re-run `dbgorilla logout`,"))
	fmt.Println(style.Warn("   or run `dbgorilla setup-ide --rotate-key` to replace it."))
}
