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
		v, c, d := resolveVersion()
		fmt.Printf("dbgorilla version %s (commit %s, built %s)\n", v, c, d)
	},
}
