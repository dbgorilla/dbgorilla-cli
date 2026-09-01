package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/auth"
	"github.com/dbgorilla/dbgorilla-cli/internal/config"
	"github.com/dbgorilla/dbgorilla-cli/internal/ide"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// checkAuthAndAPI runs the /auth/user probe and returns (ok, message, identity).
// Separated so doctor can run it concurrently with the MCP-key check.
//
// The identity is returned as well as rendered into the message because doctor
// prints the role and the ids under the check line, and reaching them from the
// message string would mean parsing back what was just formatted. On any
// failure path the returned UserInfo is the zero value; callers must only read
// it when ok is true.
func checkAuthAndAPI(cmd *cobra.Command, apiURL string) (bool, string, api.UserInfo) {
	client := newAPIClient(cmd)
	body, status, err := client.Get("/api/v0_1/auth/user")
	switch {
	case err != nil:
		return false, fmt.Sprintf("cannot reach %s: %v", apiURL, err), api.UserInfo{}
	case status == http.StatusUnauthorized:
		return false, "token expired or invalid -- run: dbgorilla login", api.UserInfo{}
	case status != http.StatusOK:
		return false, fmt.Sprintf("HTTP %d from %s", status, apiURL), api.UserInfo{}
	}
	var u api.UserInfo
	_ = json.Unmarshal(body, &u)
	return true, describeIdentity(u), u
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
			return false, "no key minted -- run: dbgorilla setup-ide"
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
	Long: `Runs three checks, plus one for each MCP client detected on this machine:
  - API URL configured (and where it came from)
  - Auth token valid (and identity)
  - MCP API key present
  - ...then one per detected client, checking its config entry

The total therefore varies by machine. If the API URL cannot be resolved,
the remaining checks are skipped -- everything else needs it.

Exits 0 on green, 1 if anything is broken.`,
	RunE: runDoctor,
}

// errDoctorFailed signals failed checks back to Cobra's error handler so the
// process exits non-zero without bypassing deferred cleanup.
var errDoctorFailed = errors.New("doctor checks failed")

func runDoctor(cmd *cobra.Command, _ []string) error {
	flagURL, _ := cmd.Flags().GetString("api-url")
	apiURL, source := config.ResolveAPIURL(flagURL)

	fmt.Println(style.Info("Checking DBGorilla setup..."))
	fmt.Println()
	allOK := true

	// Check 1: API URL. Resolution always succeeds (production default as the
	// last layer), so the check reports which URL won and where it came from.
	printCheck("API URL", true, fmt.Sprintf("%s  (source: %s)", apiURL, source))

	// Checks 2 & 3: Auth+reachability and MCP-key existence in parallel.
	// Both are independent GETs against the same host; running them
	// concurrently halves the latency of `dbgorilla doctor` on slow links.
	// The shared http.Transport (internal/api) means they reuse the TLS
	// connection rather than double-handshaking.
	tokens, _ := auth.LoadTokens()
	var (
		authMsg, keyMsg string
		authOK, keyOK   bool
		authUser        api.UserInfo
		hasTokens       = tokens != nil
	)
	if hasTokens {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			authOK, authMsg, authUser = checkAuthAndAPI(cmd, apiURL)
		}()
		go func() {
			defer wg.Done()
			keyOK, keyMsg = checkMCPKey(cmd)
		}()
		wg.Wait()
	}

	if !hasTokens {
		printCheck("Auth", false, "not signed in -- run: dbgorilla login")
		allOK = false
	} else {
		label := "Auth + API"
		if !authOK && strings.Contains(authMsg, "token expired") {
			label = "Auth"
		}
		printCheck(label, authOK, authMsg)
		if authOK {
			// doctor output exists to be pasted into a support thread, and the
			// role and the ids are the part of it that identifies anything.
			// Indented to the detail column so the block reads as detail about
			// the check above rather than as three more checks.
			printIdentityDetail(os.Stdout, authUser, strings.Repeat(" ", checkDetailColumn))
		}
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
	detected := detectInstalled()
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
					"detected (manual setup -- run: dbgorilla setup-ide --client "+a.Slug()+")")
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
		fmt.Println(style.Success("All checks passed."))
		return nil
	}
	fmt.Println(style.Error("Some checks failed. See above for details."))
	return errDoctorFailed
}

// checkDetailColumn is the printed width of everything printCheck emits before
// the detail text: two spaces of indent, the bracketed four-column status tag,
// a space, the 18-column name field, and one more space. Continuation lines
// under a check indent to here so they line up with the detail above them.
//
// The style helpers wrap the tag in colour escapes, which take no columns, so
// the visible width is the same whether or not colour is on.
const checkDetailColumn = 2 + len("[ OK ]") + 1 + 18 + 1

func printCheck(name string, ok bool, detail string) {
	tag := style.Error("FAIL")
	if ok {
		tag = style.Success(" OK ")
	}
	fmt.Printf("  [%s] %-18s %s\n", tag, name, detail)
}

// checkClientConfigured returns (ok, message) for one writer adapter.
// For Claude Code, prefers `claude mcp list` (authoritative across the
// CLI's own scope precedence); for everything else, parses the writer's
// default-scope config file directly and looks for the dbgorilla entry.
func checkClientConfigured(w ide.Writer) (bool, string) {
	if w.Slug() == "claude-code" {
		if _, err := lookPath("claude"); err == nil {
			out, err := execCommand("claude", "mcp", "list").Output()
			if err == nil && strings.Contains(strings.ToLower(string(out)), ide.MCPServerName) {
				return true, "registered (`claude mcp list`)"
			}
			return false, "not registered -- run: dbgorilla setup-ide --client claude-code"
		}
	}
	path, err := w.ConfigPath(w.DefaultScope())
	if err != nil {
		return false, "cannot resolve config path: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "no config at " + path + " -- run: dbgorilla setup-ide --client " + w.Slug()
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
