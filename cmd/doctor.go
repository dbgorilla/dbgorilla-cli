package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/spf13/cobra"
)

// checkAuthAndAPI runs the /auth/user probe and returns (ok, message).
// Separated so doctor can run it concurrently with the MCP-key check.
func checkAuthAndAPI(cmd *cobra.Command, apiURL string) (bool, string) {
	client := newAPIClient(cmd)
	body, status, err := client.Get("/api/v0_1/auth/user")
	switch {
	case err != nil:
		return false, fmt.Sprintf("cannot reach %s: %v", apiURL, err)
	case status == http.StatusUnauthorized:
		return false, "token expired or invalid -- run: dbg login"
	case status != http.StatusOK:
		return false, fmt.Sprintf("HTTP %d from %s", status, apiURL)
	}
	var u api.UserInfo
	_ = json.Unmarshal(body, &u)
	return true, fmt.Sprintf("%s  (org: %s)",
		firstNonEmpty(u.Email, u.Username),
		firstNonEmpty(u.Tenant, u.TenantID))
}

// checkMCPKey runs the MCP-key probe and returns (ok, message).
func checkMCPKey(cmd *cobra.Command) (bool, string) {
	client := newAPIClient(cmd)
	body, status, err := client.Get("/api/v0_1/client_api_keys/mcp-api-access")
	switch {
	case err != nil:
		return false, fmt.Sprintf("cannot check: %v", err)
	case status == http.StatusOK:
		var raw string
		_ = json.Unmarshal(body, &raw)
		if raw == "" {
			return false, "no key minted -- run: dbg setup-ide"
		}
		return true, "exists"
	default:
		return false, fmt.Sprintf("HTTP %d", status)
	}
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Verify the DBGorilla CLI is configured correctly",
	Long: `Runs four checks:
  - API URL configured (and where it came from)
  - Auth token valid (and identity)
  - MCP API key minted
  - Claude Code MCP entry present

Exits 0 on green, 1 if anything is broken.`,
	RunE: runDoctor,
}

// errDoctorFailed signals failed checks back to Cobra's error handler so the
// process exits non-zero without bypassing deferred cleanup.
var errDoctorFailed = errors.New("doctor checks failed")

func runDoctor(cmd *cobra.Command, _ []string) error {
	flagURL, _ := cmd.Flags().GetString("api-url")
	apiURL, source := config.ResolveAPIURL(flagURL)

	fmt.Println("Checking DBGorilla setup...")
	fmt.Println()
	allOK := true

	// Check 1: API URL configured
	if apiURL == "" {
		printCheck("API URL", false, "not configured -- run: dbg config set api-url <url>")
		fmt.Println()
		fmt.Println("Cannot continue without an API URL. Aborting remaining checks.")
		return errDoctorFailed
	}
	printCheck("API URL", true, fmt.Sprintf("%s  (source: %s)", apiURL, source))

	// Checks 2 & 3: Auth+reachability and MCP-key existence in parallel.
	// Both are independent GETs against the same host; running them
	// concurrently halves the latency of `dbg doctor` on slow links.
	// The shared http.Transport (internal/api) means they reuse the TLS
	// connection rather than double-handshaking.
	tokens, _ := auth.LoadTokens()
	var (
		authMsg, keyMsg string
		authOK, keyOK   bool
		hasTokens       = tokens != nil
	)
	if hasTokens {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			authOK, authMsg = checkAuthAndAPI(cmd, apiURL)
		}()
		go func() {
			defer wg.Done()
			keyOK, keyMsg = checkMCPKey(cmd)
		}()
		wg.Wait()
	}

	if !hasTokens {
		printCheck("Auth", false, "not signed in -- run: dbg login")
		allOK = false
	} else {
		label := "Auth + API"
		if !authOK && strings.Contains(authMsg, "token expired") {
			label = "Auth"
		}
		printCheck(label, authOK, authMsg)
		if !authOK {
			allOK = false
		}
		printCheck("MCP API key", keyOK, keyMsg)
		if !keyOK {
			allOK = false
		}
	}

	// Check 4: Per-client MCP entry presence. Iterate every detected
	// writer-type adapter and check whether the dbgorilla entry is present
	// under that client's config. Hint-only adapters (Claude Desktop) are
	// reported separately as informational.
	detected := ide.DetectInstalled()
	if len(detected) == 0 {
		printCheck("MCP clients", false,
			"no supported clients detected -- install Claude Code, Cursor, VS Code, etc.")
	} else {
		for _, a := range detected {
			label := "MCP: " + a.Name()
			switch v := a.(type) {
			case ide.Writer:
				ok, msg := checkClientConfigured(v)
				printCheck(label, ok, msg)
				if !ok {
					allOK = false
				}
			case ide.Hinter:
				printCheck(label, true,
					"detected (manual setup -- run: dbg setup-ide --client "+a.Slug()+")")
			}
		}
	}

	// Informational: warn if tokens are coming from the file fallback.
	// Not a failure -- the keychain may be locked or unavailable -- but the
	// user should know their tokens are sitting on disk in 0600 plaintext.
	if tokens != nil {
		if dir, err := config.Dir(); err == nil {
			fb := filepath.Join(dir, "credentials.json")
			if _, err := os.Stat(fb); err == nil {
				printCheck("Token storage", true, "OS keychain unavailable -- tokens in 0600 fallback file at "+fb)
			}
		}
	}

	fmt.Println()
	if allOK {
		fmt.Println("All checks passed.")
		return nil
	}
	fmt.Println("Some checks failed. See above for details.")
	return errDoctorFailed
}

func printCheck(name string, ok bool, detail string) {
	tag := "FAIL"
	if ok {
		tag = " OK "
	}
	fmt.Printf("  [%s] %-18s %s\n", tag, name, detail)
}

// checkClientConfigured returns (ok, message) for one writer adapter.
// For Claude Code, prefers `claude mcp list` (authoritative across the
// CLI's own scope precedence); for everything else, parses the writer's
// default-scope config file directly and looks for the dbgorilla entry.
func checkClientConfigured(w ide.Writer) (bool, string) {
	if w.Slug() == "claude-code" {
		if _, err := exec.LookPath("claude"); err == nil {
			out, err := exec.Command("claude", "mcp", "list").Output()
			if err == nil && strings.Contains(strings.ToLower(string(out)), ide.MCPServerName) {
				return true, "registered (`claude mcp list`)"
			}
			return false, "not registered -- run: dbg setup-ide --client claude-code"
		}
	}
	path, err := w.ConfigPath(w.DefaultScope())
	if err != nil {
		return false, "cannot resolve config path: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "no config at " + path + " -- run: dbg setup-ide --client " + w.Slug()
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, "config at " + path + " is not valid JSON"
	}
	servers, ok := cfg[w.TopLevelKey()].(map[string]any)
	if !ok {
		return false, "no " + w.TopLevelKey() + " block in " + path
	}
	if _, present := servers[ide.MCPServerName]; !present {
		return false, "no '" + ide.MCPServerName + "' entry in " + path
	}
	return true, "entry present in " + path
}
