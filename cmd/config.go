package cmd

import (
	"fmt"
	"strconv"

	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configUnsetCmd)
	rootCmd.AddCommand(configCmd)
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage CLI configuration",
	Long: `Get/set persistent CLI configuration in $XDG_CONFIG_HOME/dbgorilla/cli.toml
(defaults to ~/.config/dbgorilla/cli.toml).

Supported keys:
  api-url    The DBGorilla deployment URL.
  insecure   Whether to skip TLS verification. Persisted automatically
             when --insecure is passed to ` + "`dbg login`" + `.

The CLI also reads from a system-wide config at
/etc/dbgorilla/cli.toml (or the OS equivalent), which IT teams can
deploy via MDM. The system file takes lower priority than the user file
written here.`,
}

// --- set ----------------------------------------------------------------

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Args:  cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		key, val := args[0], args[1]
		cfg, _ := config.LoadUser()
		switch key {
		case "api-url", "api_url":
			cfg.API.URL = val
		case "insecure":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("invalid boolean for insecure %q (expected: true/false)", val)
			}
			cfg.API.Insecure = b
		default:
			return fmt.Errorf("unknown config key %q (supported: api-url, insecure)", key)
		}
		if err := cfg.SaveUser(); err != nil {
			return fmt.Errorf("cannot save config: %w", err)
		}
		fmt.Printf("Set %s = %s\n", key, val)
		return nil
	},
}

// --- get ----------------------------------------------------------------

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Show a configuration value (with the source it was resolved from)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		switch key {
		case "api-url", "api_url":
			flagURL, _ := cmd.Flags().GetString("api-url")
			url, source := config.ResolveAPIURL(flagURL)
			if url == "" {
				fmt.Println("api-url: (not set)")
				fmt.Println("  source: none")
				fmt.Println()
				fmt.Println("  Configure with:")
				fmt.Println("    dbg config set api-url https://your-deployment")
				fmt.Println("    export DBGORILLA_API_URL=https://your-deployment")
				return nil
			}
			fmt.Printf("api-url: %s\n", url)
			fmt.Printf("  source: %s\n", source)
			if source == config.SourceSystem {
				fmt.Printf("  path:   %s\n", config.SystemConfigPath())
			}
			return nil
		case "insecure":
			fmt.Printf("insecure: %t\n", resolveInsecure(cmd))
			return nil
		default:
			return fmt.Errorf("unknown config key %q (supported: api-url, insecure)", key)
		}
	},
}

// --- unset --------------------------------------------------------------

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Clear a configuration value from the user config",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		key := args[0]
		cfg, _ := config.LoadUser()
		switch key {
		case "api-url", "api_url":
			cfg.API.URL = ""
		case "insecure":
			cfg.API.Insecure = false
		default:
			return fmt.Errorf("unknown config key %q (supported: api-url, insecure)", key)
		}
		if err := cfg.SaveUser(); err != nil {
			return fmt.Errorf("cannot save config: %w", err)
		}
		fmt.Printf("Unset %s\n", key)
		return nil
	},
}
