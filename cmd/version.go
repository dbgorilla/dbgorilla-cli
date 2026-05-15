package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the CLI version",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("dbg version %s (commit %s, built %s)\n", Version, Commit, Date)
	},
}
