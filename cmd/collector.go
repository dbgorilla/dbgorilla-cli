package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/preflight"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Install-path seams. These indirect the three operations in runInstall that
// touch the Docker engine or a live database, so the install workflow can be
// exercised end-to-end in tests with fakes. Production defaults call the real
// implementations.
var (
	// dockerAvailable reports whether a usable Docker engine is reachable.
	dockerAvailable = collector.DockerAvailable
	// runPreflight runs the read-only database preflight against a DSN.
	runPreflight = preflight.Run
	// runContainer starts the collector container (`docker run`). Method
	// expression (not a wrapping closure) so the default carries no uncovered
	// statement of its own -- collector.Runner.Run is func(collector.Runner) error.
	runContainer = collector.Runner.Run
	// checkWorkload probes the workload (pg_stat_statements) capability for
	// `dbg collector status`; stubbed in tests to avoid a live DB.
	checkWorkload = preflight.CheckWorkload
)

func init() {
	installCmd.Flags().String("name", "", "Display name for this database target (prompted if omitted)")
	installCmd.Flags().String("db-host", "localhost", "Database host")
	installCmd.Flags().Int("db-port", 5432, "Database port")
	installCmd.Flags().String("db-name", "", "Comma-separated database names (empty = all databases on the server)")
	installCmd.Flags().String("db-user", "", "Read-only database user (prompted if omitted)")
	installCmd.Flags().String("db-password", "", "Database password (prompted without echo if omitted; or set "+collector.DBPasswordEnv+")")
	installCmd.Flags().String("ssl-mode", "verify-full", "libpq ssl_mode: disable, require, verify-ca, verify-full")
	installCmd.Flags().String("image", collector.DefaultImage, "Collector container image")
	installCmd.Flags().Bool("yes", false, "Skip confirmation prompts")
	installCmd.Flags().Bool("dry-run", false, "Render config and print the docker command without minting, writing, or starting anything")
	installCmd.Flags().String("auth-url", "", "Override the auth host base URL (default: collector's deployment default)")
	installCmd.Flags().String("keycloak-url", "", "Deprecated: use --auth-url")
	_ = installCmd.Flags().MarkDeprecated("keycloak-url", "use --auth-url instead")
	installCmd.Flags().String("otlp-url", "", "Override the OTLP gateway base URL")
	installCmd.Flags().String("opamp-url", "", "Override the OpAMP websocket base URL")
	installCmd.Flags().String("ca-cert", "", "Path to a PEM CA bundle to trust (for private/internal-CA deployments)")
	installCmd.Flags().Bool("force", false, "Provision even if the database preflight reports failures")

	uninstallCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")

	logsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	logsCmd.Flags().String("tail", "100", "Number of trailing log lines to show")

	collectorCmd.AddCommand(installCmd, statusCmd, listCmd, logsCmd, startCmd, stopCmd, restartCmd, uninstallCmd)
	rootCmd.AddCommand(collectorCmd)
}

var collectorCmd = &cobra.Command{
	Use:   "collector",
	Short: "Install and manage a local DBGorilla collector",
	Long: `Run the DBGorilla collector locally (in Docker) to connect a database in
your dev environment to DBGorilla.

  dbg collector install     Provision, configure, and start a collector
  dbg collector status      Show the collector's state and connection
  dbg collector logs        Tail the collector's logs
  dbg collector stop/start  Pause or resume without losing the identity
  dbg collector uninstall   Stop the collector and deprovision its identity`,
}

// --- install --------------------------------------------------------------

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Provision, configure, and start a local collector",
	RunE:  runInstall,
}

func runInstall(cmd *cobra.Command, _ []string) error {
	if dry, _ := cmd.Flags().GetBool("dry-run"); dry {
		return dryRunInstall(cmd)
	}

	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}

	// Refuse to clobber an existing install.
	if st, _ := collector.LoadState(); st != nil {
		return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
			st.AgentID)
	}

	// Environment preflight: Docker must be usable before we mint anything.
	if err := dockerAvailable(); err != nil {
		return err
	}

	client := newAPIClient(cmd)

	// Capability gate: the managed collector only exists on main-based backends.
	supported, err := client.CollectorSupported()
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	if !supported {
		return api.ErrCollectorUnsupported
	}

	// Gather the database target.
	target, err := resolveTarget(cmd)
	if err != nil {
		return err
	}
	password, err := resolveDBPassword(cmd)
	if err != nil {
		return err
	}

	// Reachability preflight from the host (the CLI runs on the host, so the
	// original loopback host is correct here, not the container rewrite).
	if err := checkReachable(target.HostDial()); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  %s", err)))
		if !confirm(cmd, "Continue anyway?") {
			return errors.New("aborted")
		}
	} else {
		fmt.Println(style.Success(fmt.Sprintf("✓ Database reachable at %s", target.HostDial())))
	}

	// Deep DB preflight (read-only) before we provision anything, so a
	// misconfigured database never gets a live collector identity.
	report := runPreflight(cmd.Context(), buildDSN(target, password))
	printPreflight(report)
	if report.Failed() {
		if force, _ := cmd.Flags().GetBool("force"); !force {
			return errors.New("database preflight failed; fix the items above, or rerun with --force")
		}
		fmt.Println(style.Warn("Continuing despite preflight failures (--force)."))
	}

	// Resolve + validate the optional CA cert before minting (fail fast).
	caCert, err := resolveCACert(cmd)
	if err != nil {
		return err
	}

	// Mint the collector identity. The user token authorizes the mint.
	fmt.Println(style.Info("Provisioning collector identity..."))
	creds, err := client.ProvisionCollector()
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))

	// Render config + materialize secrets. Endpoints come from the mint
	// response (non-prod deployments), with explicit --*-url flags overriding.
	cfg := collector.Build(creds.AgentID, creds.TenantID, target, endpointsFor(creds, cmd))
	rendered, err := cfg.Render()
	if err != nil {
		return err
	}
	configPath, _ := collector.ConfigPath()
	envPath, _ := collector.EnvPath()

	if err := collector.StoreSecrets(creds.AgentID, creds.Secret, password); err != nil {
		return err
	}
	if err := collector.WriteConfig(configPath, rendered); err != nil {
		return err
	}
	if err := collector.WriteEnvFile(envPath, creds.Secret, password); err != nil {
		return err
	}
	if collector.IsLoopback(target.Host) {
		fmt.Println(style.Success(fmt.Sprintf("✓ Rewrote %s -> %s for in-container access", target.Host, collector.DockerHostInternal)))
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Wrote config: %s", configPath)))

	// Resolve the collector image: explicit --image wins; else the version the
	// deployment blesses (preferred_collector_version); else the CLI default.
	image, imageSource := resolveImage(cmd, creds)
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector image: %s (%s)", image, imageSource)))

	// Pin to an immutable digest before running, so a deployment-blessed version
	// (a bare tag) is as reproducible and tamper-evident as the hard-pinned
	// default. Already-pinned refs pass through untouched.
	pinned, err := collector.PinnedRef(image)
	if err != nil {
		return fmt.Errorf("resolving collector image digest: %w", err)
	}
	if pinned != image {
		fmt.Println(style.Success(fmt.Sprintf("✓ Pinned to digest: %s", pinned)))
	}
	image = pinned

	// Run the container.
	runner := collector.Runner{
		Name:        collector.DefaultContainerName,
		Image:       image,
		ConfigPath:  configPath,
		EnvFilePath: envPath,
		CACertPath:  caCert,
	}
	fmt.Println(style.Info(fmt.Sprintf("Starting collector container (%s)...", image)))
	if err := runContainer(runner); err != nil {
		// Roll back the just-minted identity so a failed start never orphans it.
		fmt.Println(style.Warn("Container failed to start; rolling back the provisioned identity..."))
		if derr := client.DeleteCollector(creds.AgentID); derr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not auto-deprovision %s: %v (remove it from the console)", creds.AgentID, derr)))
		}
		collector.ClearSecrets(creds.AgentID)
		_ = os.Remove(configPath)
		_ = os.Remove(envPath)
		return fmt.Errorf("%w\n\nRolled back. Fix Docker and re-run `dbg collector install`", err)
	}

	// Persist state.
	state := &collector.State{
		AgentID:       creds.AgentID,
		TenantID:      creds.TenantID,
		Domain:        creds.Domain,
		ContainerName: runner.Name,
		Image:         image,
		ConfigPath:    configPath,
		EnvFilePath:   envPath,
		CACertPath:    caCert,
		TargetName:    target.Name,
		CreatedAt:     time.Now().UTC(),
	}
	if err := collector.SaveState(state); err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Container started: %s", runner.Name)))

	// Verify connection (best-effort; not fatal).
	verifyConnection(client, creds.AgentID, caCert)

	fmt.Println()
	fmt.Println("Collector installed. Next:")
	fmt.Println("  dbg collector status     # check connection")
	fmt.Println("  dbg collector logs -f    # watch it work")
	return nil
}

// dryRunInstall renders the config and prints the docker command the real
// install would run, without contacting the backend, writing files, storing
// secrets, or starting a container. Identity fields are placeholders.
func dryRunInstall(cmd *cobra.Command) error {
	target, err := resolveTarget(cmd)
	if err != nil {
		return err
	}
	cfg := collector.Build("<minted-on-install>", "<minted-on-install>", target, endpointsFromFlags(cmd))
	rendered, err := cfg.Render()
	if err != nil {
		return err
	}
	configPath, _ := collector.ConfigPath()
	envPath, _ := collector.EnvPath()
	image, _ := cmd.Flags().GetString("image")
	caCert, _ := cmd.Flags().GetString("ca-cert")
	if caCert != "" {
		if abs, err := filepath.Abs(caCert); err == nil {
			caCert = abs
		}
	}
	runner := collector.Runner{
		Name:        collector.DefaultContainerName,
		Image:       image,
		ConfigPath:  configPath,
		EnvFilePath: envPath,
		CACertPath:  caCert,
	}

	fmt.Println(style.Info("DRY RUN — nothing was provisioned, written, or started."))
	fmt.Println()
	if collector.IsLoopback(target.Host) {
		fmt.Println(style.Info(fmt.Sprintf("Would rewrite %s -> %s for in-container access.", target.Host, collector.DockerHostInternal)))
	}
	fmt.Println(style.Info(fmt.Sprintf("Would write config:   %s", configPath)))
	fmt.Println(style.Info(fmt.Sprintf("Would write env-file: %s (0600; %s, %s)", envPath, collector.SecretEnv, collector.DBPasswordEnv)))
	fmt.Println()
	fmt.Println(style.Info("--- collector.toml ---"))
	fmt.Print(rendered)
	fmt.Println(style.Info("--- docker command ---"))
	fmt.Println(runner.RunCommandString())
	return nil
}

// --- status ---------------------------------------------------------------

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the local collector's state and connection",
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, _ []string) error {
	st, err := collector.LoadState()
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Println(style.Warn("No collector installed. Run: dbg collector install"))
		return nil
	}

	runner := collector.Runner{Name: st.ContainerName}
	exists, running, _ := runner.Running()

	fmt.Printf("Agent:      %s\n", st.AgentID)
	fmt.Printf("Tenant:     %s\n", st.TenantID)
	fmt.Printf("Target:     %s\n", st.TargetName)
	fmt.Printf("Image:      %s\n", st.Image)
	fmt.Printf("Config:     %s\n", st.ConfigPath)
	switch {
	case !exists:
		fmt.Println(style.Error("Container:  missing (run `dbg collector install` again or `dbg collector start`)"))
	case running:
		fmt.Println(style.Success("Container:  running"))
	default:
		fmt.Println(style.Warn("Container:  stopped (run `dbg collector start`)"))
	}

	// Live control-plane status (best-effort).
	if _, err := requireLogin(); err == nil {
		client := newAPIClient(cmd)
		cs, err := client.FetchCollectorStatus(st.AgentID)
		switch {
		case err != nil:
			fmt.Println(style.Warn(fmt.Sprintf("Connection: unknown (%v)", err)))
		case cs == nil:
			fmt.Println(style.Warn("Connection: not yet seen by control plane"))
		default:
			fmt.Println(style.Success(fmt.Sprintf("Connection: %s", orUnknown(cs.Status))))
		}
	}

	// Workload capability (best-effort): re-probe pg_stat_statements in the
	// `postgres` maintenance DB the collector uses to gate workload, so the
	// "topology works but the Queries views are empty" case is diagnosable here
	// rather than only at install time (dbgorilla-cli#14).
	printWorkloadStatus(cmd.Context(), st)
	return nil
}

// printWorkloadStatus recovers the monitored target from the installed config +
// keychain and prints whether the workload (pg_stat_statements) capability is
// satisfied. Entirely best-effort: any missing piece just omits the line.
func printWorkloadStatus(ctx context.Context, st *collector.State) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := collector.LoadConfig(st.ConfigPath)
	if err != nil || len(cfg.Component) == 0 {
		return
	}
	_, dbPassword, err := collector.LoadSecrets(st.AgentID)
	if err != nil {
		return
	}
	comp := cfg.Component[0]
	target := collector.Target{
		Host:      collector.DialHost(comp.Connect.Host), // host-side view
		Port:      comp.Connect.Port,
		Databases: comp.Connect.Databases,
		User:      comp.Auth.User,
		SSLMode:   comp.Connect.SSLMode,
	}
	// Bound the probe so status never hangs on an unreachable DB.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := checkWorkload(ctx, buildDSN(target, dbPassword))
	switch res.Severity {
	case preflight.OK:
		fmt.Println(style.Success("Workload:   collecting (pg_stat_statements in the postgres maintenance DB)"))
	case preflight.Warn:
		fmt.Println(style.Warn(fmt.Sprintf("Workload:   unknown (%s)", res.Detail)))
	default:
		fmt.Println(style.Error(fmt.Sprintf("Workload:   NOT collecting -- %s", res.Detail)))
		for _, f := range res.Fix {
			fmt.Printf("            %s\n", f)
		}
	}
}

// --- list -----------------------------------------------------------------

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all collectors registered to your tenant",
	RunE:  runList,
}

func runList(cmd *cobra.Command, _ []string) error {
	if _, err := requireAPIURL(cmd); err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}
	client := newAPIClient(cmd)
	items, err := client.ListCollectors()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("No collectors registered to your tenant.")
		return nil
	}

	// Note which agent is the one installed locally, if any.
	localAgent := ""
	if st, _ := collector.LoadState(); st != nil {
		localAgent = st.AgentID
	}

	fmt.Println(style.Info(fmt.Sprintf("%-38s  %-10s  %s", "AGENT ID", "STATUS", "DETAIL")))
	for _, it := range items {
		id := firstString(it, "agent_id", "id", "agentId")
		status := firstString(it, "status", "state", "connection_status")
		detail := firstString(it, "name", "hostname", "instance_id")
		marker := ""
		if id != "" && id == localAgent {
			marker = "  (this machine)"
		}
		fmt.Printf("%-38s  %-10s  %s%s\n", orUnknown(id), orUnknown(status), detail, marker)
	}
	return nil
}

// firstString returns the first non-empty string value among the given keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// --- logs / lifecycle -----------------------------------------------------

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show the collector's logs",
	RunE: func(cmd *cobra.Command, _ []string) error {
		runner, err := runnerFromState()
		if err != nil {
			return err
		}
		follow, _ := cmd.Flags().GetBool("follow")
		tail, _ := cmd.Flags().GetString("tail")
		return runner.Logs(follow, tail)
	},
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the stopped collector container",
	RunE: func(_ *cobra.Command, _ []string) error {
		runner, err := runnerFromState()
		if err != nil {
			return err
		}
		if err := runner.Start(); err != nil {
			return err
		}
		fmt.Println(style.Success("✓ Started"))
		return nil
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the collector container (identity is preserved)",
	RunE: func(_ *cobra.Command, _ []string) error {
		runner, err := runnerFromState()
		if err != nil {
			return err
		}
		if err := runner.Stop(); err != nil {
			return err
		}
		fmt.Println(style.Success("✓ Stopped"))
		return nil
	},
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the collector container",
	RunE: func(_ *cobra.Command, _ []string) error {
		runner, err := runnerFromState()
		if err != nil {
			return err
		}
		if err := runner.Restart(); err != nil {
			return err
		}
		fmt.Println(style.Success("✓ Restarted"))
		return nil
	},
}

// --- uninstall ------------------------------------------------------------

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop the collector and deprovision its identity",
	RunE:  runUninstall,
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	st, err := collector.LoadState()
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Println(style.Warn("No collector installed."))
		return nil
	}
	if !confirm(cmd, fmt.Sprintf("Remove collector %s and deprovision its identity?", st.AgentID)) {
		return errors.New("aborted")
	}

	// Stop+remove the container (best-effort).
	runner := collector.Runner{Name: st.ContainerName}
	if err := runner.Remove(); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not remove container: %v", err)))
	} else {
		fmt.Println(style.Success("✓ Container removed"))
	}

	// Deprovision the identity on the backend.
	deprovisioned := false
	if _, err := requireLogin(); err == nil {
		client := newAPIClient(cmd)
		if err := client.DeleteCollector(st.AgentID); err != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not deprovision identity: %v", err)))
		} else {
			fmt.Println(style.Success("✓ Identity deprovisioned"))
			deprovisioned = true
		}
	} else {
		fmt.Println(style.Warn("⚠  not logged in; cannot deprovision the identity."))
	}

	// Keep local state if deprovision did not succeed, so the user can retry
	// after re-logging in rather than orphaning the identity.
	if !deprovisioned {
		fmt.Println()
		fmt.Printf("The container was removed, but identity %s is still provisioned.\n", st.AgentID)
		fmt.Println("Run `dbg login`, then `dbg collector uninstall` again to deprovision it.")
		return nil
	}

	// Clear local secrets, env-file, config, state.
	collector.ClearSecrets(st.AgentID)
	_ = os.Remove(st.EnvFilePath)
	_ = os.Remove(st.ConfigPath)
	_ = collector.RemoveState()
	fmt.Println(style.Success("✓ Local config and secrets cleared"))
	return nil
}

// --- helpers --------------------------------------------------------------

func runnerFromState() (collector.Runner, error) {
	st, err := collector.LoadState()
	if err != nil {
		return collector.Runner{}, err
	}
	if st == nil {
		return collector.Runner{}, errors.New("no collector installed. Run: dbg collector install")
	}
	return collector.Runner{
		Name:        st.ContainerName,
		Image:       st.Image,
		ConfigPath:  st.ConfigPath,
		EnvFilePath: st.EnvFilePath,
		CACertPath:  st.CACertPath,
	}, nil
}

// buildDSN constructs a libpq URL for the preflight connection from the host's
// perspective (the original host, not the container rewrite). Uses the first
// configured database, or "postgres" when monitoring all databases.
func buildDSN(t collector.Target, password string) string {
	db := "postgres"
	if len(t.Databases) > 0 {
		db = t.Databases[0]
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(t.User, password),
		Host:   net.JoinHostPort(t.Host, strconv.Itoa(t.Port)),
		Path:   "/" + db,
	}
	q := url.Values{}
	if t.SSLMode != "" {
		q.Set("sslmode", t.SSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// printPreflight renders a preflight report with severity markers + fixes.
func printPreflight(rep preflight.Report) {
	for _, r := range rep.Results {
		mark := style.Success("✓")
		switch r.Severity {
		case preflight.Warn:
			mark = style.Warn("⚠")
		case preflight.Fail:
			mark = style.Error("✗")
		}
		fmt.Printf("%s %s — %s\n", mark, r.Name, r.Detail)
		for _, f := range r.Fix {
			fmt.Printf("    %s\n", f)
		}
	}
}

// resolveCACert validates --ca-cert and returns an absolute path (or "").
func resolveCACert(cmd *cobra.Command) (string, error) {
	p, _ := cmd.Flags().GetString("ca-cert")
	if p == "" {
		return "", nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("--ca-cert: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("--ca-cert: %w", err)
	}
	return abs, nil
}

// resolveImage picks the collector image to run, and a human-readable source.
// Precedence: an explicit --image flag (operator override) > the version the
// deployment blesses via preferred_collector_version > the CLI's built-in
// default. Surfacing the source makes "which version am I running?" answerable.
func resolveImage(cmd *cobra.Command, creds *api.CollectorCredentials) (image, source string) {
	if cmd.Flags().Changed("image") {
		v, _ := cmd.Flags().GetString("image")
		return v, "--image override"
	}
	if creds != nil && creds.PreferredCollectorVersion != "" {
		return collector.ImageForVersion(creds.PreferredCollectorVersion),
			"version " + creds.PreferredCollectorVersion + " blessed by deployment"
	}
	v, _ := cmd.Flags().GetString("image") // the flag's built-in default
	return v, "CLI default"
}

// endpointsFromFlags reads the optional endpoint overrides. Empty values fall
// back to the collector's built-in production defaults. (Phase 2 replaces these
// flags with values from the deployment's .well-known discovery doc.)
func endpointsFromFlags(cmd *cobra.Command) collector.Endpoints {
	otlp, _ := cmd.Flags().GetString("otlp-url")
	opamp, _ := cmd.Flags().GetString("opamp-url")
	return collector.Endpoints{
		AuthBaseURL:  authURLFlag(cmd),
		OtlpBaseURL:  otlp,
		OpampBaseURL: opamp,
	}
}

// authURLFlag returns the auth host override, preferring --auth-url and falling
// back to the deprecated --keycloak-url so existing scripts keep working.
func authURLFlag(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString("auth-url"); v != "" {
		return v
	}
	v, _ := cmd.Flags().GetString("keycloak-url")
	return v
}

// endpointsFor resolves the collector endpoints: start from the mint response
// (backend supplies these for non-prod deployments), then let any explicit
// --*-url flag override. Empty fields fall back to the collector's prod
// defaults.
func endpointsFor(creds *api.CollectorCredentials, cmd *cobra.Command) collector.Endpoints {
	e := collector.Endpoints{
		AuthBaseURL:  creds.AuthHost(),
		OtlpBaseURL:  creds.OtlpBaseURL,
		OpampBaseURL: creds.OpampBaseURL,
	}
	if v := authURLFlag(cmd); v != "" {
		e.AuthBaseURL = v
	}
	if v, _ := cmd.Flags().GetString("otlp-url"); v != "" {
		e.OtlpBaseURL = v
	}
	if v, _ := cmd.Flags().GetString("opamp-url"); v != "" {
		e.OpampBaseURL = v
	}
	// The OTLP exporter needs an explicit host:port; a bare "https://host"
	// (no port) makes otelcol fail with "missing port in address". Default to
	// the scheme's standard port when the endpoint omits one.
	e.OtlpBaseURL = withDefaultPort(e.OtlpBaseURL)
	return e
}

// withDefaultPort adds the scheme's standard port to a URL whose host omits one.
// Leaves the value untouched if it already has a port, is empty, or is unparseable.
func withDefaultPort(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Port() != "" {
		return raw
	}
	switch u.Scheme {
	case "https":
		u.Host += ":443"
	case "http":
		u.Host += ":80"
	default:
		return raw
	}
	return u.String()
}

func resolveTarget(cmd *cobra.Command) (collector.Target, error) {
	host, _ := cmd.Flags().GetString("db-host")
	port, _ := cmd.Flags().GetInt("db-port")
	user, _ := cmd.Flags().GetString("db-user")
	name, _ := cmd.Flags().GetString("name")
	sslMode, _ := cmd.Flags().GetString("ssl-mode")
	dbList, _ := cmd.Flags().GetString("db-name")

	if user == "" {
		user = prompt("Read-only database user", "")
	}
	if user == "" {
		return collector.Target{}, errors.New("a database user is required")
	}
	if name == "" {
		name = prompt("Name for this target", user)
	}

	var databases []string
	for _, d := range strings.Split(dbList, ",") {
		if s := strings.TrimSpace(d); s != "" {
			databases = append(databases, s)
		}
	}

	return collector.Target{
		Name:      name,
		Host:      host,
		Port:      port,
		Databases: databases,
		User:      user,
		SSLMode:   sslMode,
	}, nil
}

func resolveDBPassword(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("db-password"); p != "" {
		return p, nil
	}
	if p := os.Getenv(collector.DBPasswordEnv); p != "" {
		return p, nil
	}
	fmt.Print("Database password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("cannot read password: %w", err)
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" {
		return "", errors.New("a database password is required")
	}
	return pw, nil
}

// checkReachable does a quick TCP dial so the most common local-dev failure
// (DB not running / wrong port) is caught before we provision anything.
func checkReachable(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach database at %s: %v\n   Check the host/port, that Postgres is running, and that it accepts TCP connections", addr, err)
	}
	_ = conn.Close()
	return nil
}

// verifyConnection polls the control plane briefly for the collector to come
// online. Non-fatal: a slow first connect is normal.
func verifyConnection(client *api.Client, agentID, caCert string) {
	fmt.Print(style.Info("Verifying connection"))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		fmt.Print(".")
		cs, err := client.FetchCollectorStatus(agentID)
		if err == nil && cs != nil && isConnected(cs.Status) {
			fmt.Println()
			fmt.Println(style.Success(fmt.Sprintf("✓ Collector connected (%s)", cs.Status)))
			return
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Println()
	fmt.Println(style.Warn("⚠  Not connected yet — this can take a moment on first start."))
	fmt.Println("   Check `dbg collector status` and `dbg collector logs -f`.")
	if caCert == "" {
		fmt.Println("   If your deployment uses an internal/private CA, re-run with")
		fmt.Println("   --ca-cert /path/to/ca.pem so the collector trusts its endpoints.")
	}
}

func isConnected(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "connected", "online", "ready", "ok":
		return true
	}
	return false
}

func prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func confirm(cmd *cobra.Command, question string) bool {
	if yes, _ := cmd.Flags().GetBool("yes"); yes {
		return true
	}
	fmt.Printf("%s [y/N]: ", question)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
