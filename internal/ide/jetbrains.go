package ide

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// JetBrains covers every JetBrains IDE (IntelliJ IDEA, PhpStorm, PyCharm,
// WebStorm, GoLand, DataGrip, RubyMine, CLion, Rider and the rest) through
// the AI Assistant plugin they share. AI Assistant reads its MCP servers
// from one JSON file per user, ~/.ai/mcp/mcp.json, and one per project,
// <project>/.ai/mcp/mcp.json, so a single entry serves every product and
// version on the machine. The format is the usual {"mcpServers": {...}}
// with "type", "url" and "headers" for a remote server.
//
// That file is read by AI Assistant 2025.3 and later. Earlier versions kept
// their servers in each IDE's own options/llm.mcpServers.xml, an
// undocumented layout that changed between releases, so this adapter never
// writes it: a user on an older IDE is told to paste the entry through the
// settings dialog instead (see Warnings).
//
// Junie, JetBrains's coding agent, runs inside AI Assistant's chat and sees
// the same servers once "Pass custom MCP servers" is on in its settings. It
// reads skills from .junie/skills, which is where the DBGorilla skill goes.
type JetBrains struct{}

func (j *JetBrains) Name() string { return "JetBrains IDEs (AI Assistant)" }
func (j *JetBrains) Slug() string { return "jetbrains" }

// Detect reports a JetBrains IDE when any product has a configuration
// directory on this machine, when an IDE application bundle is installed
// (macOS, including Toolbox installs under ~/Applications), or when AI
// Assistant's own config file already exists.
func (j *JetBrains) Detect() bool {
	if len(j.installedProducts()) > 0 {
		return true
	}
	if exists(filepath.Join(homeDir(), ".ai", "mcp", "mcp.json")) {
		return true
	}
	if goos == "darwin" {
		for _, root := range jetBrainsAppRoots() {
			if len(jetBrainsAppBundles(root)) > 0 {
				return true
			}
		}
	}
	return false
}

func (j *JetBrains) SupportedScopes() []Scope { return []Scope{ScopeUser, ScopeProject} }
func (j *JetBrains) DefaultScope() Scope      { return ScopeUser }

// ConfigPath:
//   - User: ~/.ai/mcp/mcp.json, shared by every JetBrains IDE on the machine.
//   - Project: .ai/mcp/mcp.json in cwd. AI Assistant reads it only once the
//     project is marked trusted in the IDE.
func (j *JetBrains) ConfigPath(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".ai", "mcp", "mcp.json"), nil
	}
	return filepath.Join(homeDir(), ".ai", "mcp", "mcp.json"), nil
}

func (j *JetBrains) TopLevelKey() string { return "mcpServers" }

// BuildEntry uses "streamable-http", the type AI Assistant's own
// documentation shows for a remote server. A plain "url" is accepted too,
// but naming the transport keeps the IDE from probing for SSE first.
func (j *JetBrains) BuildEntry(mcpURL, apiKey string) map[string]any {
	return map[string]any{
		"type": "streamable-http",
		"url":  mcpURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + apiKey,
		},
	}
}

// SkillDir: ~/.junie/skills/dbgorilla (user) or .junie/skills/dbgorilla
// (project). Junie reads both from inside the IDE and from its CLI.
func (j *JetBrains) SkillDir(scope Scope) (string, error) {
	return skillDir(scope, []string{".junie", "skills"}, []string{".junie", "skills"})
}

// Warnings lists what the user has to do by hand.
//
// The first is always true: AI Assistant lists a server it finds in mcp.json
// but leaves it switched off until someone ticks it, unless "Automatically
// enable new and changed MCP servers" is already on. Without this line the
// setup looks finished and the tools never appear.
//
// The second is derived from the IDE configuration directories on this
// machine: an IDE older than 2025.3 has an AI Assistant that does not read
// mcp.json, so the entry has to go in through its settings dialog.
func (j *JetBrains) Warnings() []string {
	out := []string{
		"AI Assistant adds dbgorilla switched off. In each JetBrains IDE: Settings > Tools >\n" +
			"   AI Assistant > Model Context Protocol (MCP), tick dbgorilla, then Apply.",
	}
	var old []string
	for _, p := range j.installedProducts() {
		if p.year < 2025 || (p.year == 2025 && p.minor < 3) {
			old = append(old, p.name+p.version)
		}
	}
	if len(old) == 0 {
		return out
	}
	return append(out,
		"AI Assistant in "+strings.Join(old, ", ")+" predates mcp.json and will not see this entry.\n"+
			"   In that IDE: Settings > Tools > AI Assistant > Model Context Protocol (MCP) > Add,\n"+
			"   and paste the output of `dbgorilla setup-ide --print-config --client jetbrains`.")
}

// jetBrainsProduct is one <Product><Version> configuration directory, such
// as PhpStorm2025.2 or IdeaIC2024.3.
type jetBrainsProduct struct {
	name    string // directory prefix, e.g. "PhpStorm"
	version string // "2025.2"
	year    int
	minor   int
}

// jetBrainsProductPrefixes are the configuration directory names JetBrains
// uses, which differ from the marketing names (IntelliJ IDEA Ultimate is
// IntelliJIdea, Community is IdeaIC, PyCharm Community is PyCharmCE).
var jetBrainsProductPrefixes = []string{
	"IntelliJIdea", "IdeaIC", "PhpStorm", "PyCharm", "PyCharmCE", "WebStorm",
	"GoLand", "DataGrip", "RubyMine", "CLion", "Rider", "RustRover", "Aqua",
}

// jetBrainsConfigRoot is the directory that holds one configuration
// directory per installed product and version. It is where "Directories
// used by the IDE" puts it on each platform.
func jetBrainsConfigRoot() string {
	home := homeDir()
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "JetBrains")
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "JetBrains")
		}
		return filepath.Join(home, "AppData", "Roaming", "JetBrains")
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "JetBrains")
		}
		return filepath.Join(home, ".config", "JetBrains")
	}
}

// installedProducts lists the product configuration directories present,
// newest first. A directory survives an uninstall, so this says which IDEs
// have run here, which is what the warnings need.
func (j *JetBrains) installedProducts() []jetBrainsProduct {
	entries, err := os.ReadDir(jetBrainsConfigRoot())
	if err != nil {
		return nil
	}
	var out []jetBrainsProduct
	for _, e := range entries {
		if p, ok := parseJetBrainsProduct(e.Name()); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].year != out[b].year {
			return out[a].year > out[b].year
		}
		return out[a].minor > out[b].minor
	})
	return out
}

// parseJetBrainsProduct splits "PhpStorm2025.2" into its product and
// version. The longest matching prefix wins so PyCharmCE is not read as
// PyCharm with a version of "CE2025.2".
func parseJetBrainsProduct(dir string) (jetBrainsProduct, bool) {
	best := ""
	for _, p := range jetBrainsProductPrefixes {
		if strings.HasPrefix(dir, p) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return jetBrainsProduct{}, false
	}
	ver := dir[len(best):]
	year, minor, ok := strings.Cut(ver, ".")
	if !ok {
		return jetBrainsProduct{}, false
	}
	y, err1 := strconv.Atoi(year)
	m, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil || y < 2000 {
		return jetBrainsProduct{}, false
	}
	return jetBrainsProduct{name: best, version: ver, year: y, minor: m}, true
}

// jetBrainsAppRoots are the macOS directories IDE bundles are installed in:
// the system Applications folder for the standalone installers, and the
// user's for Toolbox. A variable so tests do not see the machine's own IDEs.
var jetBrainsAppRoots = func() []string {
	return []string{"/Applications", filepath.Join(homeDir(), "Applications")}
}

// jetBrainsAppBundles lists the JetBrains IDE application bundles in root,
// matched by the product name at the start of the bundle name so that
// "IntelliJ IDEA CE.app" and "PyCharm Community Edition.app" count too.
func jetBrainsAppBundles(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	names := []string{
		"IntelliJ IDEA", "PhpStorm", "PyCharm", "WebStorm", "GoLand",
		"DataGrip", "RubyMine", "CLion", "Rider", "RustRover", "Aqua",
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".app") {
			continue
		}
		for _, p := range names {
			if strings.HasPrefix(n, p) {
				out = append(out, filepath.Join(root, n))
				break
			}
		}
	}
	return out
}
