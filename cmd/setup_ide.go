package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/spf13/cobra"
)

func init() {
	setupIDECmd.Flags().StringSlice("client",
		nil, "Comma-separated list of clients to configure (default: all detected). "+
			"Run with --list-clients to see options.")
	setupIDECmd.Flags().String("scope", "",
		"Config scope: user or project. Defaults to each client's preferred scope.")
	setupIDECmd.Flags().Bool("list-clients", false,
		"List supported clients (and which are detected on this system).")
	setupIDECmd.Flags().Bool("dry-run", false,
		"Show what would be written without modifying any files.")
	setupIDECmd.Flags().Bool("print-config", false,
		"Print the MCP entry for each selected client (no write, but still calls the API for the key).")
	setupIDECmd.Flags().Bool("print-key", false,
		"Print the MCP API key only (for paste-elsewhere flows).")
	setupIDECmd.Flags().Bool("print-admin-allowlist", false,
		"Print the IT-facing snippet for the Claude admin console allowlist.")
	setupIDECmd.Flags().Bool("no-claude-cli", false,
		"For Claude Code: skip `claude mcp add`, write the config file directly.")
	rootCmd.AddCommand(setupIDECmd)
}

var setupIDECmd = &cobra.Command{
	Use:   "setup-ide",
	Short: "Configure IDE/agent clients to connect to DBGorilla via MCP",
	Long: `Configures one or more MCP clients to connect to your DBGorilla deployment.

By default, auto-detects every supported client installed on this machine
and configures each one. Use --client to target specific tools, or
--list-clients to see what's supported and what's currently detected.

Supported writable clients:
  claude-code, cursor, vscode, opencode, gemini

Detect-only clients (printed manual instructions):
  claude-desktop  (HTTP MCP requires Settings -> Connectors UI flow)

Use --print-admin-allowlist to get the IT-facing snippet to send to
whoever manages your Claude admin console (app.claude.com / Team or
Enterprise tiers).`,
	RunE: runSetupIDE,
}

func runSetupIDE(cmd *cobra.Command, _ []string) error {
	// --list-clients works without auth or API URL -- it's pure local
	// detection. Short-circuit before requireAPIURL.
	if listClients, _ := cmd.Flags().GetBool("list-clients"); listClients {
		printClientList()
		return nil
	}

	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}

	// --print-admin-allowlist short-circuits the auth + API key flow.
	if printAdmin, _ := cmd.Flags().GetBool("print-admin-allowlist"); printAdmin {
		printAdminAllowlist(apiURL)
		return nil
	}

	if _, err := requireLogin(); err != nil {
		return err
	}

	mcpURL := strings.TrimRight(apiURL, "/") + "/mcp/"

	// Resolve which adapters to act on.
	clientFlag, _ := cmd.Flags().GetStringSlice("client")
	selected, err := resolveSelectedAdapters(clientFlag)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Println("No supported clients detected on this system.")
		fmt.Println("Supported clients (none currently detected):")
		for _, a := range ide.Registry {
			fmt.Printf("  - %s (%s)\n", a.Name(), a.Slug())
		}
		fmt.Println("\nInstall one of the above, or pass --client <slug> to configure")
		fmt.Println("a client even when auto-detect can't see it.")
		return nil
	}

	scopeFlag, _ := cmd.Flags().GetString("scope")
	scopeOverride, err := parseScope(scopeFlag)
	if err != nil {
		return err
	}

	client := newAPIClient(cmd)

	// --print-key still needs the API key but no per-client work.
	mcpKey, err := fetchMCPKey(client)
	if err != nil {
		return err
	}
	if printKey, _ := cmd.Flags().GetBool("print-key"); printKey {
		fmt.Println(mcpKey)
		return nil
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	printConfig, _ := cmd.Flags().GetBool("print-config")
	noClaudeCLI, _ := cmd.Flags().GetBool("no-claude-cli")

	configured := 0
	hinted := 0
	failed := 0

	for _, adapter := range selected {
		// Hint-only adapters (Claude Desktop): print instructions and move on.
		if h, ok := adapter.(ide.Hinter); ok {
			if _, isWriter := adapter.(ide.Writer); !isWriter {
				fmt.Printf("\n--- %s (manual setup) ---\n", adapter.Name())
				fmt.Println(h.Hint(mcpURL))
				hinted++
				continue
			}
		}

		writer, ok := adapter.(ide.Writer)
		if !ok {
			// Adapter is neither Writer nor Hinter -- shouldn't happen, but
			// don't crash.
			fmt.Printf("\n--- %s ---\nNo setup path implemented; skipping.\n", adapter.Name())
			continue
		}

		fmt.Printf("\n--- %s ---\n", adapter.Name())

		scope := pickScope(writer, scopeOverride)

		if printConfig {
			if err := emitPrintConfig(writer, mcpURL, mcpKey); err != nil {
				fmt.Printf("error: %v\n", err)
				failed++
			}
			continue
		}

		// Claude Code's preferred path is `claude mcp add` (handles managed-org
		// allowlists). Skip the file writer if the CLI is on PATH.
		if writer.Slug() == "claude-code" && !noClaudeCLI {
			if _, lookErr := exec.LookPath("claude"); lookErr == nil {
				if dryRun {
					fmt.Printf("Would run: claude mcp add (scope=%s, name=dbgorilla)\n", scope)
					configured++
					continue
				}
				if err := claudeMCPAdd(mcpURL, mcpKey, string(scope)); err != nil {
					failed++
					fmt.Printf("error: %v\n", interpretClaudeError(err, apiURL))
					continue
				}
				fmt.Println("✓ Registered via `claude mcp add`")
				configured++
				continue
			}
			fmt.Println("Note: `claude` CLI not on PATH; falling back to direct config-file write.")
		}

		path, err := writer.ConfigPath(scope)
		if err != nil {
			failed++
			fmt.Printf("error resolving config path: %v\n", err)
			continue
		}

		if dryRun {
			fmt.Printf("Would write MCP entry to: %s (scope=%s)\n", path, scope)
			configured++
			continue
		}

		res, err := ide.WriteMCPConfig(writer, mcpURL, mcpKey, scope)
		if err != nil {
			failed++
			if errors.Is(err, ide.ErrJSONCRefused) {
				fmt.Printf("Refused to overwrite JSONC config at %s.\n", path)
				fmt.Println("Run `dbg setup-ide --print-config --client " + writer.Slug() +
					"` and paste the output into the file manually.")
				continue
			}
			fmt.Printf("error: %v\n", err)
			continue
		}
		switch {
		case res.NoOp:
			fmt.Printf("Up to date: %s\n", res.Path)
		case res.Updated:
			fmt.Printf("✓ Updated existing dbgorilla entry: %s\n", res.Path)
			fmt.Printf("  Backup: %s\n", res.BackupPath)
		case res.Created:
			fmt.Printf("✓ Created %s\n", res.Path)
		default:
			fmt.Printf("✓ Merged dbgorilla entry into %s\n", res.Path)
			if res.BackupPath != "" {
				fmt.Printf("  Backup: %s\n", res.BackupPath)
			}
		}
		configured++
	}

	fmt.Println()
	fmt.Printf("Done. Configured: %d, Hinted: %d, Failed: %d.\n", configured, hinted, failed)

	// TLS warning shown once at the end if applicable.
	if resolveInsecure(cmd) {
		fmt.Println()
		fmt.Println("⚠  Your deployment uses an internal certificate.")
		fmt.Println("   Node-based clients (Claude Code, opencode) may reject the MCP")
		fmt.Println("   connection without NODE_EXTRA_CA_CERTS set:")
		fmt.Println("     export NODE_EXTRA_CA_CERTS=/path/to/internal-ca.pem")
	}

	if failed > 0 {
		return fmt.Errorf("%d client(s) failed to configure", failed)
	}
	return nil
}

// resolveSelectedAdapters maps the --client flag (or auto-detect) to a list
// of Adapter instances to act on.
func resolveSelectedAdapters(clientFlag []string) ([]ide.Adapter, error) {
	if len(clientFlag) == 0 {
		return ide.DetectInstalled(), nil
	}
	var out []ide.Adapter
	var unknown []string
	for _, slug := range clientFlag {
		s := strings.TrimSpace(slug)
		if s == "" {
			continue
		}
		a := ide.Find(s)
		if a == nil {
			unknown = append(unknown, s)
			continue
		}
		out = append(out, a)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown client(s): %s. Run `dbg setup-ide --list-clients`",
			strings.Join(unknown, ", "))
	}
	return out, nil
}

// parseScope normalises the --scope flag value. Empty string means
// "use each client's default."
func parseScope(s string) (ide.Scope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "user":
		return ide.ScopeUser, nil
	case "project":
		return ide.ScopeProject, nil
	default:
		return "", fmt.Errorf("invalid --scope %q (expected: user or project)", s)
	}
}

// pickScope chooses the scope for one writer: explicit override if it's
// supported by the writer, otherwise the writer's default.
func pickScope(w ide.Writer, override ide.Scope) ide.Scope {
	if override == "" {
		return w.DefaultScope()
	}
	for _, s := range w.SupportedScopes() {
		if s == override {
			return override
		}
	}
	return w.DefaultScope()
}

// emitPrintConfig prints the JSON entry for the writer to stdout, using
// the writer's actual top-level key.
func emitPrintConfig(w ide.Writer, mcpURL, apiKey string) error {
	entry := w.BuildEntry(mcpURL, apiKey)
	blob, err := json.MarshalIndent(map[string]any{
		w.TopLevelKey(): map[string]any{ide.MCPServerName: entry},
	}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(blob))
	return nil
}

// printClientList shows every registered client, whether it's detected,
// and what scopes/paths it would target.
func printClientList() {
	fmt.Println("Supported MCP clients:")
	fmt.Println()
	for _, a := range ide.Registry {
		mark := " "
		if a.Detect() {
			mark = "✓"
		}
		role := "writer"
		if _, isWriter := a.(ide.Writer); !isWriter {
			role = "manual setup"
		}
		fmt.Printf("  [%s] %-20s %s   (%s)\n", mark, a.Slug(), a.Name(), role)
		if w, ok := a.(ide.Writer); ok {
			scopes := make([]string, 0, len(w.SupportedScopes()))
			for _, s := range w.SupportedScopes() {
				scopes = append(scopes, string(s))
			}
			fmt.Printf("           scopes: %s, default: %s, key: %q\n",
				strings.Join(scopes, ", "), w.DefaultScope(), w.TopLevelKey())
		}
	}
	fmt.Println()
	fmt.Println("[✓] = detected on this system")
}

// claudeMCPAdd shells out to `claude mcp add` with the right scope flag.
// Returns nil on success; the raw exec error otherwise (caller interprets
// for known failure modes like managed-allowlist policy blocks).
//
// Argument ordering is load-bearing: `--header` is variadic (consumes every
// following token until the next flag) per `claude mcp add --help`. Putting
// it after the positional <name> and <commandOrUrl> matches the documented
// example and prevents the header from eating the URL.
//
// Security note: the bearer token appears on argv for the lifetime of the
// `claude` subprocess (visible to the same-uid user via `ps`). The Claude
// CLI offers no env/stdin alternative for headers as of v0.1.0; until it
// does, this is unavoidable. On error we explicitly redact the Bearer token
// from the captured stderr before returning so a verbose Claude error
// doesn't echo the secret back onto the dev's terminal.
func claudeMCPAdd(mcpURL, apiKey, scope string) error {
	args := []string{
		"mcp", "add",
		"--scope", scope,
		"--transport", "http",
		ide.MCPServerName, mcpURL,
		"--header", "Authorization: Bearer " + apiKey,
	}
	out, err := exec.Command("claude", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("`claude mcp add` failed: %w\n%s", err, redactBearer(string(out), apiKey))
	}
	return nil
}

// redactBearer replaces any literal occurrence of the API key (and the
// generic "Bearer <token>" pattern) in the captured subprocess output with
// "***". Defense against a verbose Claude CLI error message echoing the
// argv. Cheap and conservative.
func redactBearer(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "***")
	}
	s = bearerPattern.ReplaceAllString(s, "Bearer ***")
	return s
}

var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+\S+`)

// interpretClaudeError translates a `claude mcp add` failure into helpful
// guidance.
func interpretClaudeError(err error, _ string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		_ = exitErr.ExitCode()
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "policy") ||
		strings.Contains(msg, "allowlist") ||
		strings.Contains(msg, "denied") ||
		strings.Contains(msg, "not permitted") {
		return fmt.Errorf(
			"setup blocked by your Claude org's allowlist policy.\n\n"+
				"Run:  dbg setup-ide --print-admin-allowlist\n"+
				"...and send the output to whoever manages your Claude admin console.\n\n"+
				"(underlying error: %v)", err)
	}
	return err
}

// validScope is kept for backwards-compat in tests.
func validScope(s string) bool {
	switch s {
	case "local", "user", "project":
		return true
	}
	return false
}

// printAdminAllowlist outputs the IT-facing snippet for the Claude admin
// console at app.claude.com. Self-contained; no auth required.
func printAdminAllowlist(apiURL string) {
	mcpURL := strings.TrimRight(apiURL, "/") + "/mcp/"
	fmt.Println("To allowlist DBGorilla in your Claude admin console:")
	fmt.Println()
	fmt.Printf("  Server name:  %s\n", ide.MCPServerName)
	fmt.Printf("  Server URL:   %s\n", mcpURL)
	fmt.Println("  Transport:    HTTP")
	fmt.Println("  Auth header:  Authorization: Bearer <each-developer's-API-key>")
	fmt.Println()
	fmt.Println("Steps for your Claude admin:")
	fmt.Println("  1. Sign in to https://app.claude.com/admin")
	fmt.Println("  2. Settings → Code → MCP servers → Allowed servers → + Add")
	fmt.Println("  3. Paste the values above")
	fmt.Println("  4. Save")
	fmt.Println()
	fmt.Println("Once approved, each developer runs `dbg setup-ide` to wire it in.")
}

// fetchMCPKey calls the backend to get or create an MCP API key. Response
// is a JSON-encoded string (e.g. `"abc123"`); falls back to a bare string
// for resilience against minor backend variations.
func fetchMCPKey(client *api.Client) (string, error) {
	body, status, err := client.Post("/api/v0_1/client_api_keys/mcp-api-access", nil)
	if err != nil {
		return "", fmt.Errorf("cannot mint MCP key: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("backend returned HTTP %d when minting MCP key:\n%s", status, string(body))
	}
	if len(body) == 0 {
		return "", fmt.Errorf("backend returned empty MCP key body")
	}
	var raw string
	if err := json.Unmarshal(body, &raw); err == nil && raw != "" {
		return raw, nil
	}
	// Fallback: trim quotes from a bare string with no JSON envelope.
	s := strings.TrimSpace(string(body))
	s = strings.Trim(s, `"`)
	if s == "" {
		return "", fmt.Errorf("backend returned empty MCP key body")
	}
	return s, nil
}
