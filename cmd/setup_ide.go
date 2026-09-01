package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// Testability seams. These indirect the few operations in the cmd package that
// touch the host environment (spawning the `claude`/`brew` CLIs, probing PATH,
// and auto-detecting installed IDEs) so tests can stub them for hermetic,
// deterministic runs. Production defaults are the real stdlib/ide functions.
var (
	// execCommand builds an *exec.Cmd. Stubbed in tests to fake the `claude`
	// and `brew` subprocesses without a real binary on PATH.
	execCommand = exec.Command
	// lookPath reports whether a binary is on PATH. Stubbed to control whether
	// the `claude` CLI appears installed.
	lookPath = exec.LookPath
	// detectInstalled returns the IDE adapters present on the system. Stubbed
	// so IDE detection is deterministic regardless of the host's installs.
	detectInstalled = ide.DetectInstalled
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
	setupIDECmd.Flags().Bool("rotate-key", false,
		"Issue a new MCP API key, invalidating the current one everywhere it is configured.")
	setupIDECmd.Flags().Bool("remove", false,
		"Undo setup: strip the dbgorilla MCP entry (and skill) from the selected clients.")
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

For Claude Code, this also installs a short DBGorilla skill, so the agent
knows to check the live database through these tools before reasoning about
it from the source alone.

Use --remove to undo all of that for the selected clients. It touches only
local files and leaves the API key alone; dbgorilla logout revokes the key.

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

	// --remove is pure local file work: it needs no deployment URL, no token
	// and no API key, and it has to keep working after `logout` or after the
	// deployment has gone away. Short-circuit before anything asks for those.
	if remove, _ := cmd.Flags().GetBool("remove"); remove {
		return runRemoveIDE(cmd)
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
	rotate, _ := cmd.Flags().GetBool("rotate-key")
	mcpKey, err := fetchMCPKey(client, rotate)
	if err != nil {
		return err
	}
	if rotate {
		fmt.Println(style.Warn("Issued a new MCP API key. Any client still holding the previous"))
		fmt.Println(style.Warn("one will be rejected until you re-run setup-ide there."))
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
				fmt.Println()
				fmt.Println(style.Info(fmt.Sprintf("--- %s (manual setup) ---", adapter.Name())))
				fmt.Println(h.Hint(mcpURL))
				hinted++
				continue
			}
		}

		writer, ok := adapter.(ide.Writer)
		if !ok {
			// Adapter is neither Writer nor Hinter -- shouldn't happen, but
			// don't crash.
			fmt.Println()
			fmt.Println(style.Info(fmt.Sprintf("--- %s ---", adapter.Name())))
			fmt.Println("No setup path implemented; skipping.")
			continue
		}

		fmt.Println()
		fmt.Println(style.Info(fmt.Sprintf("--- %s ---", adapter.Name())))

		scope := pickScope(writer, scopeOverride)

		if printConfig {
			if err := emitPrintConfig(writer, mcpURL, mcpKey); err != nil {
				fmt.Println(style.Error(fmt.Sprintf("error: %v", err)))
				failed++
			}
			continue
		}

		// Claude Code's preferred path is `claude mcp add` (handles managed-org
		// allowlists). Skip the file writer if the CLI is on PATH.
		if writer.Slug() == "claude-code" && !noClaudeCLI {
			if _, lookErr := lookPath("claude"); lookErr == nil {
				if dryRun {
					fmt.Println(style.Info(fmt.Sprintf("Would run: claude mcp add (scope=%s, name=dbgorilla)", scope)))
					installSkill(scope, dryRun)
					configured++
					continue
				}
				if err := claudeMCPAdd(mcpURL, mcpKey, string(scope)); err != nil {
					failed++
					fmt.Println(style.Error(fmt.Sprintf("error: %v", interpretClaudeError(err, apiURL))))
					continue
				}
				fmt.Println(style.Success("✓ Registered via `claude mcp add`"))
				installSkill(scope, dryRun)
				configured++
				continue
			}
			fmt.Println("Note: `claude` CLI not on PATH; falling back to direct config-file write.")
		}

		path, err := writer.ConfigPath(scope)
		if err != nil {
			failed++
			fmt.Println(style.Error(fmt.Sprintf("error resolving config path: %v", err)))
			continue
		}

		if dryRun {
			fmt.Println(style.Info(fmt.Sprintf("Would write MCP entry to: %s (scope=%s)", path, scope)))
			configured++
			continue
		}

		res, err := ide.WriteMCPConfig(writer, mcpURL, mcpKey, scope)
		if err != nil {
			failed++
			if errors.Is(err, ide.ErrJSONCRefused) {
				fmt.Println(style.Warn(fmt.Sprintf("Refused to overwrite JSONC config at %s.", path)))
				fmt.Println("Run `dbgorilla setup-ide --print-config --client " + writer.Slug() +
					"` and paste the output into the file manually.")
				continue
			}
			fmt.Println(style.Error(fmt.Sprintf("error: %v", err)))
			continue
		}
		switch {
		case res.NoOp:
			fmt.Printf("Up to date: %s\n", res.Path)
		case res.Updated:
			fmt.Println(style.Success(fmt.Sprintf("✓ Updated existing dbgorilla entry: %s", res.Path)))
			fmt.Printf("  Backup: %s\n", res.BackupPath)
		case res.Created:
			fmt.Println(style.Success(fmt.Sprintf("✓ Created %s", res.Path)))
		default:
			fmt.Println(style.Success(fmt.Sprintf("✓ Merged dbgorilla entry into %s", res.Path)))
			if res.BackupPath != "" {
				fmt.Printf("  Backup: %s\n", res.BackupPath)
			}
		}
		if writer.Slug() == "claude-code" {
			installSkill(scope, dryRun)
		}
		configured++
	}

	fmt.Println()
	summary := fmt.Sprintf("Done. Configured: %d, Hinted: %d, Failed: %d.", configured, hinted, failed)
	if failed > 0 {
		fmt.Println(style.Error(summary))
	} else {
		fmt.Println(style.Success(summary))
	}

	// TLS warning shown once at the end if applicable.
	if resolveInsecure(cmd) {
		fmt.Println()
		fmt.Println(style.Warn("⚠  Your deployment uses an internal certificate."))
		fmt.Println(style.Warn("   Node-based clients (Claude Code, opencode) may reject the MCP"))
		fmt.Println(style.Warn("   connection without NODE_EXTRA_CA_CERTS set:"))
		fmt.Println(style.Warn("     export NODE_EXTRA_CA_CERTS=/path/to/internal-ca.pem"))
	}

	if failed > 0 {
		return fmt.Errorf("%d client(s) failed to configure", failed)
	}
	return nil
}

// runRemoveIDE undoes what setup-ide wrote: the MCP entry for each selected
// client, and the skill where one was installed.
//
// It stops short of revoking the API key. The key is one per user and shared
// by every client, so revoking it here would break the editors the user did
// not name -- the same failure this command exists to make reversible. The key
// is `logout`'s to destroy, because that is the point at which the user is
// saying they are done with the deployment entirely.
func runRemoveIDE(cmd *cobra.Command) error {
	clientFlag, _ := cmd.Flags().GetStringSlice("client")
	selected, err := resolveSelectedAdapters(clientFlag)
	if err != nil {
		return err
	}
	scopeFlag, _ := cmd.Flags().GetString("scope")
	scopeOverride, err := parseScope(scopeFlag)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	removed, failed := 0, 0
	for _, adapter := range selected {
		writer, ok := adapter.(ide.Writer)
		if !ok {
			continue // detect-only clients were configured by hand; leave them alone
		}
		fmt.Println()
		fmt.Println(style.Info(fmt.Sprintf("--- %s ---", writer.Name())))
		scope := pickScope(writer, scopeOverride)

		if dryRun {
			if path, err := writer.ConfigPath(scope); err == nil {
				fmt.Println(style.Info(fmt.Sprintf("Would remove the dbgorilla entry from: %s", path)))
			}
			if writer.Slug() == "claude-code" {
				if dir, err := ide.SkillDir(scope); err == nil {
					fmt.Println(style.Info("Would remove the DBGorilla skill from: " + dir))
				}
			}
			removed++
			continue
		}

		// Claude Code's own CLI owns the registration when it is present, so
		// ask it to undo it. A "not found" is the outcome we want anyway, so
		// the result is ignored; the file sweep below catches whatever the CLI
		// did not, including entries written when it was not installed.
		//
		// --scope has to be passed and has to match the one setup used: `claude
		// mcp remove` defaults to local scope, so a scopeless call looks in the
		// wrong place for anything registered with `--scope user` or `project`
		// and reports "not found" while the entry stays.
		if writer.Slug() == "claude-code" {
			if _, lookErr := lookPath("claude"); lookErr == nil {
				_ = execCommand("claude", "mcp", "remove", "--scope", string(scope), ide.MCPServerName).Run()
			}
		}

		res, err := ide.RemoveMCPConfig(writer, scope)
		switch {
		case err != nil:
			failed++
			fmt.Println(style.Error(fmt.Sprintf("error: %v", err)))
			continue
		case res.Absent:
			fmt.Printf("Nothing to remove: %s\n", res.Path)
		default:
			fmt.Println(style.Success("✓ Removed the dbgorilla entry from " + res.Path))
			fmt.Printf("  Backup: %s\n", res.BackupPath)
		}

		if writer.Slug() == "claude-code" {
			skill, err := ide.RemoveSkill(scope)
			switch {
			case err != nil:
				fmt.Println(style.Warn(fmt.Sprintf("Could not remove the DBGorilla skill: %v", err)))
			case skill.Absent:
				// Nothing installed; say nothing.
			default:
				fmt.Println(style.Success("✓ Removed the DBGorilla skill"))
			}
		}
		removed++
	}

	fmt.Println()
	if removed == 0 && failed == 0 {
		fmt.Println("No supported clients selected; nothing to remove.")
		return nil
	}
	summary := fmt.Sprintf("Done. Cleaned: %d, Failed: %d.", removed, failed)
	if failed > 0 {
		fmt.Println(style.Error(summary))
		return fmt.Errorf("%d client(s) could not be cleaned up", failed)
	}
	fmt.Println(style.Success(summary))
	fmt.Println()
	fmt.Println("The MCP API key is untouched and still valid. `dbgorilla logout` revokes it.")
	return nil
}

// resolveSelectedAdapters maps the --client flag (or auto-detect) to a list
// of Adapter instances to act on.
func resolveSelectedAdapters(clientFlag []string) ([]ide.Adapter, error) {
	if len(clientFlag) == 0 {
		return detectInstalled(), nil
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
		return nil, fmt.Errorf("unknown client(s): %s. Run `dbgorilla setup-ide --list-clients`",
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
	fmt.Println(style.Info("Supported MCP clients:"))
	fmt.Println()
	for _, a := range ide.Registry {
		mark := " "
		if a.Detect() {
			mark = style.Success("✓")
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
	fmt.Println("[" + style.Success("✓") + "] = detected on this system")
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
	// `claude mcp add` errors ("already exists") if an entry with this name is
	// present, so it is not idempotent on its own. Remove any existing entry
	// first (best-effort; a "not found" is expected and ignored) so re-running
	// setup-ide -- or running it after a stale/prior entry -- reliably updates
	// in place instead of failing, matching the write-in-place behavior of the
	// other IDE writers.
	_ = execCommand("claude", "mcp", "remove", ide.MCPServerName).Run()
	args := []string{
		"mcp", "add",
		"--scope", scope,
		"--transport", "http",
		ide.MCPServerName, mcpURL,
		"--header", "Authorization: Bearer " + apiKey,
	}
	out, err := execCommand("claude", args...).CombinedOutput()
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
				"Run:  dbgorilla setup-ide --print-admin-allowlist\n"+
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
	fmt.Println(style.Info("To allowlist DBGorilla in your Claude admin console:"))
	fmt.Println()
	fmt.Printf("  Server name:  %s\n", ide.MCPServerName)
	fmt.Printf("  Server URL:   %s\n", mcpURL)
	fmt.Println("  Transport:    HTTP")
	fmt.Println("  Auth header:  Authorization: Bearer <each-developer's-API-key>")
	fmt.Println()
	fmt.Println(style.Info("Steps for your Claude admin:"))
	fmt.Println("  1. Sign in to https://app.claude.com/admin")
	fmt.Println("  2. Settings → Code → MCP servers → Allowed servers → + Add")
	fmt.Println("  3. Paste the values above")
	fmt.Println("  4. Save")
	fmt.Println()
	fmt.Println("Once approved, each developer runs `dbgorilla setup-ide` to wire it in.")
}

// installSkill drops the DBGorilla skill next to the MCP registration, so
// Claude Code knows to consult the tools it now has rather than waiting to be
// told. Wiring up the server without it leaves the tools present but unused
// on exactly the work they exist for.
//
// A failure here is reported and stepped over. The MCP registration is the
// part the user asked for and it has already succeeded; an unwritable skills
// directory should not turn a working setup into a failed command.
func installSkill(scope ide.Scope, dryRun bool) {
	if dryRun {
		dir, err := ide.SkillDir(scope)
		if err == nil {
			fmt.Println(style.Info("Would install the DBGorilla skill to: " + dir))
		}
		return
	}
	res, err := ide.InstallSkill(scope)
	switch {
	case err != nil:
		fmt.Println(style.Warn(fmt.Sprintf("Could not install the DBGorilla skill: %v", err)))
	case res.NoOp:
		fmt.Printf("Skill up to date: %s\n", res.Path)
	case res.Updated:
		fmt.Println(style.Success("✓ Updated the DBGorilla skill: " + res.Path))
	default:
		fmt.Println(style.Success("✓ Installed the DBGorilla skill: " + res.Path))
	}
}

// mcpKeyPath is the backend resource holding this user's MCP API key.
const mcpKeyPath = "/api/v0_1/client_api_keys/mcp-api-access"

// fetchMCPKey returns the MCP API key to configure clients with, reusing the
// existing one unless the caller explicitly asked for a new one.
//
// The order of those two operations is the whole point. Minting is
// destructive: the backend issues a fresh secret and overwrites the stored
// one, and there is exactly one key per user, so a mint does not "get or
// create" -- it rotates, and every copy of the previous key stops working the
// instant it returns. Anyone who had the key in a second editor, or in
// anything outside their editor, would find it rejected later, at use, with
// nothing at setup time having said so. Reading first means the ordinary case
// -- configuring one more client, or re-running this command -- leaves every
// client already configured still working.
func fetchMCPKey(client *api.Client, rotate bool) (string, error) {
	if !rotate {
		if key := lookupMCPKey(client); key != "" {
			return key, nil
		}
	}
	body, status, err := client.Post(mcpKeyPath, nil)
	if err != nil {
		return "", fmt.Errorf("cannot mint MCP key: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("backend returned HTTP %d when minting MCP key:\n%s", status, string(body))
	}
	if key := decodeMCPKey(body); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("backend returned empty MCP key body")
}

// lookupMCPKey returns the key already issued to this user, or "" when there
// is nothing to reuse.
//
// Failures are deliberately swallowed rather than returned. Every reason this
// can fail -- unreachable deployment, expired token, a backend too old to
// serve the read -- is a reason the mint that follows will fail too, and the
// mint produces the message the user can act on. Returning an error here
// would replace that with a worse one for the same underlying problem.
func lookupMCPKey(client *api.Client) string {
	body, status, err := client.Get(mcpKeyPath)
	if err != nil || status != http.StatusOK {
		return ""
	}
	return decodeMCPKey(body)
}

// decodeMCPKey reads the key out of a response body. It is a JSON-encoded
// string (e.g. `"abc123"`); the bare-string fallback is resilience against
// minor backend variations. Returns "" when there is no key in the body.
func decodeMCPKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw string
	if err := json.Unmarshal(body, &raw); err == nil {
		return raw
	}
	return strings.Trim(strings.TrimSpace(string(body)), `"`)
}
