package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(upgradeCmd)
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Update dbg to the latest version",
	Long: `Updates the dbg binary in place.

If installed via Homebrew, runs ` + "`brew upgrade dbgorilla/tap/dbg`" + `.
Otherwise, prints the right one-line command for your install method so
you can run it. The CLI does NOT replace itself in place (security: the
binary that just downloaded shouldn't decide it's safe to overwrite).`,
	RunE: runUpgrade,
}

func runUpgrade(_ *cobra.Command, _ []string) error {
	if installedViaBrew() {
		fmt.Println("Detected Homebrew install. Running: brew upgrade dbgorilla/tap/dbg")
		c := exec.Command("brew", "upgrade", "dbgorilla/tap/dbg")
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	// Not a brew install. Find where the binary lives and tell the user the
	// matching upgrade command for the path they used.
	self, err := os.Executable()
	if err != nil {
		self = "/usr/local/bin/dbg"
	}
	fmt.Printf("dbg %s is installed at %s\n", Version, self)
	fmt.Println()
	fmt.Println("Not a Homebrew install. Re-run the install command for your environment:")
	fmt.Println()
	fmt.Println("  # On-prem (curl from your DBGorilla backend):")
	fmt.Println("    curl -fsSL https://<your-deployment>/install.sh | sh")
	fmt.Println()
	fmt.Println("  # Or download a binary from GitHub Releases and replace the file:")
	fmt.Println("    https://github.com/dbgorilla/dbgorilla-cli/releases/latest")
	fmt.Println()
	fmt.Println("In-place self-update is intentionally not supported -- it would let a")
	fmt.Println("compromised binary write its own replacement. Use your package manager.")
	return nil
}

// installedViaBrew is a heuristic: the binary's path is somewhere under
// Homebrew's Cellar prefix. Works for /opt/homebrew (Apple Silicon) and
// /usr/local (Intel macOS, Linuxbrew).
func installedViaBrew() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	for _, prefix := range []string{"/opt/homebrew/", "/usr/local/Cellar/", "/home/linuxbrew/"} {
		if len(self) > len(prefix) && self[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
