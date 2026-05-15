package cmd

import (
	"fmt"

	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(logoutCmd)
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear stored credentials",
	RunE: func(_ *cobra.Command, _ []string) error {
		if err := auth.ClearTokens(); err != nil {
			return fmt.Errorf("failed to clear credentials: %w", err)
		}
		fmt.Println("Signed out.")
		return nil
	},
}
