package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/spf13/cobra"
)

func init() {
	whoamiCmd.Flags().Bool("json", false, "Emit identity as JSON")
	rootCmd.AddCommand(whoamiCmd)
}

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the signed-in user and organization",
	RunE:  runWhoami,
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	if _, err := requireAPIURL(cmd); err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}

	client := newAPIClient(cmd)
	body, status, err := client.Get("/api/v0_1/auth/user")
	if err != nil {
		return fmt.Errorf("cannot reach API: %w", err)
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("token expired or invalid -- run: dbg login")
	}
	if status != http.StatusOK {
		return fmt.Errorf("unexpected response (HTTP %d)", status)
	}
	var u api.UserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return fmt.Errorf("cannot parse identity: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out, _ := json.MarshalIndent(u, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	// On release-202603.007 the /auth/user response leaves `tenant` and
	// `user_id` empty and populates only `tenant_id`. Fall back to the
	// non-empty fields; omit user-id when nothing meaningful is available.
	identity := firstNonEmpty(u.Email, u.Username)
	org := firstNonEmpty(u.Tenant, u.TenantID)
	if u.UserID != "" {
		fmt.Printf("%s  (org: %s, user-id: %s)\n", identity, org, u.UserID)
	} else {
		fmt.Printf("%s  (org: %s)\n", identity, org)
	}
	return nil
}
