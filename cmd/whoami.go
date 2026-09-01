package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
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
		return fmt.Errorf("token expired or invalid -- run: dbgorilla login")
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

	// Nobody runs `whoami` in passing. It is run to answer an identity
	// question, and the answer usually gets pasted into a support thread or an
	// issue -- where the ids are the part that identifies anything. Putting
	// them behind a flag would mean the one command whose whole job is
	// answering "who am I" gives back the least useful half unless you already
	// know a flag exists. So this command prints all of it, and `login` --
	// which people run to get past it, not to read it -- prints one line.
	fmt.Println(style.Success(describeIdentity(u)))
	printIdentityDetail(cmd.OutOrStdout(), u, "  ")
	return nil
}

// describeIdentity renders the one-line answer to "who am I": the account,
// and the organization by the name its members call it.
//
// The organization's UUID is deliberately not on this line. It is an internal
// identifier, it is the same on every line of every command, and it crowds
// out the one word the reader is actually looking for. `whoami` prints it
// underneath on its own line; `login` keeps it under --verbose.
//
// Deployments old enough not to send an organization name fall back to the
// UUID, because showing the raw id beats showing "(org: )".
func describeIdentity(u api.UserInfo) string {
	who := firstNonEmpty(u.Email, u.Username)
	org := firstNonEmpty(u.Organization, u.TenantID)
	if org == "" {
		return who
	}
	return fmt.Sprintf("%s  (org: %s)", who, org)
}

// printIdentityDetail writes the identifiers a human does not need but a
// support conversation does. Each line is omitted when the deployment does
// not supply it, so an older backend prints a shorter block rather than a
// block full of blanks.
//
// indent is the prefix for each line. Callers pass whatever lines the block up
// under the identity above it: a plain two spaces for `whoami`, and the width
// of doctor's check gutter for `doctor`, where anything at the left margin
// would read as another check rather than as detail about the last one.
func printIdentityDetail(w io.Writer, u api.UserInfo, indent string) {
	for _, row := range []struct{ label, value string }{
		{"role", u.Role},
		{"user-id", u.UserID},
		{"org-id", u.TenantID},
	} {
		if row.value != "" {
			_, _ = fmt.Fprintf(w, "%s%-9s %s\n", indent, row.label+":", row.value)
		}
	}
}
