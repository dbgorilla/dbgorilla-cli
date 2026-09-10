package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// The NetApp Instaclustr source: `dbg collector install --provider instaclustr
// --cluster-id <id>` discovers the managed PostgreSQL cluster through the
// Instaclustr Cluster Management API, creates a read-only monitoring role,
// allowlists the collector's egress IP on the cluster firewall, and runs the
// collector — locally in Docker today. A *source* is deliberately not a
// *target*: where the collector runs (--target) stays independent of where
// the database lives, which is why none of the lifecycle commands branch on
// it.
//
// Two API keys with two fates: the provisioning key does the setup from this
// machine (discovery, firewall writes, reading the default user's password to
// create the role) and is never persisted; the read-only key is the only one
// that ships with the collector, for node discovery.

// Env fallbacks, checked when the flags are absent (prompted for last).
const (
	instaclustrUserEnv         = "INSTACLUSTR_USERNAME"
	instaclustrProvisioningEnv = "INSTACLUSTR_PROVISIONING_API_KEY"
	instaclustrReadOnlyEnv     = "INSTACLUSTR_READONLY_API_KEY"
)

// Test seams, one var per side effect (the aws seams' pattern).
var (
	discoverInstaclustr   = collector.DiscoverInstaclustrCluster
	ensureFirewallRule    = collector.EnsureFirewallRule
	deleteFirewallRule    = collector.DeleteInstaclustrFirewallRule
	createInstaclustrRole = collector.EnsureInstaclustrRole
	publicEgressIP        = func(ctx context.Context) (string, error) { return collector.PublicEgressIP(ctx) }
)

func init() {
	installCmd.Flags().String("provider", "", "Database source: 'instaclustr' for a NetApp Instaclustr managed PostgreSQL cluster (default: the database named by --db-host)")
	installCmd.Flags().String("cluster-id", "", "Instaclustr cluster id (from the console URL or Cluster Details)")
	installCmd.Flags().String("instaclustr-user", "", "Instaclustr console username (or "+instaclustrUserEnv+")")
	installCmd.Flags().String("instaclustr-api-key", "", "Instaclustr provisioning API key, used for setup on this machine only (or "+instaclustrProvisioningEnv+")")
	installCmd.Flags().String("instaclustr-readonly-key", "", "Instaclustr READ-ONLY provisioning API key the collector keeps for discovery (or "+instaclustrReadOnlyEnv+")")
	installCmd.Flags().Bool("use-private-addresses", false, "Dial the cluster's private node addresses (VPC-peered collectors)")
	installCmd.Flags().String("allow-ip", "", "Public IP the firewall should allow for the collector (default: this machine's, auto-detected)")

	refreshFirewallCmd.Flags().String("instaclustr-user", "", "Instaclustr console username (or "+instaclustrUserEnv+")")
	refreshFirewallCmd.Flags().String("instaclustr-api-key", "", "Instaclustr provisioning API key (or "+instaclustrProvisioningEnv+")")
	refreshFirewallCmd.Flags().String("allow-ip", "", "Public IP to allowlist (default: this machine's, auto-detected)")
	collectorCmd.AddCommand(refreshFirewallCmd)
}

// instaclustrSource reports whether this install targets an Instaclustr
// cluster. runInstall consults it before its --target dispatch.
func instaclustrSource(cmd *cobra.Command) bool {
	p, _ := cmd.Flags().GetString("provider")
	return strings.EqualFold(p, "instaclustr")
}

// runInstallInstaclustr is the install flow for the instaclustr source. Only
// the docker target runs it today; the aws target needs the Fargate template
// to carry the read-only key as a third secret first.
func runInstallInstaclustr(cmd *cobra.Command) error {
	if target, _ := cmd.Flags().GetString("target"); target != "" && target != "docker" && target != "local" {
		return fmt.Errorf("--provider instaclustr currently supports --target docker only. "+
			"Run the collector in Docker on any host with a stable public IP (that IP goes on "+
			"the cluster's firewall allowlist); --target %s support is coming", target)
	}
	// The docker CA mount replaces the container's system trust store, which
	// the collector's own control-plane TLS relies on — so a cluster CA can't
	// ride it. verify-full support waits on a bundling story for both roots.
	if ca, _ := cmd.Flags().GetString("ca-cert"); ca != "" {
		return errors.New("--ca-cert is not supported with --provider instaclustr yet: the docker CA " +
			"mount would replace the system trust store the collector's own TLS needs. " +
			"The install uses ssl_mode=require (encrypted, unverified) for now")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}
	if st, _ := collector.LoadState(); st != nil {
		return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
			st.AgentID)
	}
	if !dryRun {
		if err := dockerAvailable(); err != nil {
			return err
		}
	}

	clusterID, _ := cmd.Flags().GetString("cluster-id")
	if clusterID == "" {
		if !interactiveTerminal() {
			return errors.New("--cluster-id is required with --provider instaclustr. " +
				"Find it in the Instaclustr console URL or Cluster Details")
		}
		clusterID = strings.TrimSpace(prompt("Instaclustr cluster id", ""))
		if clusterID == "" {
			return errors.New("aborted: no cluster id given")
		}
	}
	setupCreds, err := resolveInstaclustrCreds(cmd, "", "instaclustr-api-key", instaclustrProvisioningEnv,
		"Instaclustr provisioning API key (setup only, never stored)")
	if err != nil {
		return err
	}
	readOnlyKey := "preview" // never rendered; the config carries an env reference
	if !dryRun {
		readOnlyKey, err = resolveInstaclustrKey(cmd, "instaclustr-readonly-key", instaclustrReadOnlyEnv,
			"Instaclustr READ-ONLY API key (the collector keeps this one)")
		if err != nil {
			return err
		}
	}

	client := newAPIClient(cmd)
	supported, err := client.CollectorSupported()
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	if !supported {
		return api.ErrCollectorUnsupported
	}

	// Discover the cluster through the Cluster Management API (read-only, so
	// the dry-run path shares it).
	ctx := cmd.Context()
	ict, err := discoverInstaclustr(ctx, setupCreds, clusterID)
	if err != nil {
		return err
	}
	usePrivate, _ := cmd.Flags().GetBool("use-private-addresses")
	seedHost := ""
	for _, n := range ict.Nodes {
		if h := n.Host(usePrivate); h != "" {
			seedHost = h
			break
		}
	}
	if seedHost == "" {
		return errors.New("no node has an address on the selected network side. " +
			"A private-network cluster needs --use-private-addresses; a public one must not set it")
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Cluster %q: %d node(s), PostgreSQL %s, %s %s",
		ict.Name, len(ict.Nodes), ict.PostgresVersion, ict.CloudProvider, ict.Region)))

	allowCIDR := ""
	if raw, _ := cmd.Flags().GetString("allow-ip"); raw != "" {
		if allowCIDR, err = collector.AllowCIDR(raw); err != nil {
			return err
		}
	} else {
		ip, err := publicEgressIP(ctx)
		if err != nil {
			return err
		}
		if allowCIDR, err = collector.AllowCIDR(ip); err != nil {
			return err
		}
	}

	sslMode := ""
	if cmd.Flags().Changed("ssl-mode") {
		sslMode, _ = cmd.Flags().GetString("ssl-mode")
	}
	dbNames, _ := cmd.Flags().GetString("db-name")
	databases := splitCSV(dbNames)

	if dryRun {
		return dryRunInstaclustr(cmd, ict, seedHost, databases, sslMode, setupCreds.Username, usePrivate, allowCIDR)
	}

	// Firewall: the collector runs on THIS machine for the docker target, so
	// one rule covers both the setup connection and the collector. Every
	// failure below that follows a rule WE created rolls the rule back —
	// otherwise a later re-run finds it "pre-existing", never records
	// ownership, and refresh-firewall accumulates stale rules forever.
	rule, created, err := ensureFirewallRule(ctx, setupCreds, clusterID, allowCIDR)
	if err != nil {
		return err
	}
	rollbackRule := func() {
		if created {
			if derr := deleteFirewallRule(ctx, setupCreds, rule.ID); derr != nil {
				fmt.Println(style.Warn(fmt.Sprintf("⚠  could not remove the firewall rule created for %s: %v (remove it from the console)", allowCIDR, derr)))
			}
		}
	}
	if created {
		fmt.Println(style.Success(fmt.Sprintf("✓ Firewall: allowlisted %s on the cluster", allowCIDR)))
	} else {
		fmt.Println(style.Success(fmt.Sprintf("✓ Firewall: %s already allowlisted", allowCIDR)))
	}

	// Ensure the read-only monitoring role via the cluster's default user —
	// ALTERing the password when it already exists, because this run's fresh
	// password is what ships to the collector. The rule may take a moment to
	// pass packets, so connection failures (only) get retries.
	monitorPassword, err := collector.GenerateInstaclustrPassword()
	if err != nil {
		rollbackRule()
		return err
	}
	dsn := collector.InstaclustrAdminDSN(seedHost, 5432, ict.DefaultUserPassword)
	if err := ensureRoleWithRetry(ctx, dsn, collector.InstaclustrMonitorUser, monitorPassword); err != nil {
		rollbackRule()
		return fmt.Errorf("%w\n\nA just-created firewall rule can take ~a minute to apply; re-running is safe", err)
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Monitoring role %q ready (pg_monitor + pg_read_all_data)", collector.InstaclustrMonitorUser)))

	// Deep DB preflight with the monitoring role itself — the same gate the
	// local path runs, so a cluster missing pg_stat_statements is a warning
	// before anything is provisioned, not a silent gap after.
	monitorDSN := collector.InstaclustrAdminDSNAs(collector.InstaclustrMonitorUser, monitorPassword, seedHost, 5432)
	report := runPreflight(ctx, monitorDSN)
	printPreflight(report)
	if report.Failed() {
		if force, _ := cmd.Flags().GetBool("force"); !force {
			rollbackRule()
			return errors.New("database preflight failed; fix the items above, or rerun with --force")
		}
		fmt.Println(style.Warn("Continuing despite preflight failures (--force)."))
	}

	caCert := "" // refused above; kept as a named value for the Runner below

	fmt.Println(style.Info("Provisioning collector identity..."))
	creds, err := client.ProvisionCollector()
	if err != nil {
		rollbackRule()
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))

	comp := collector.BuildInstaclustrComponent(ict, seedHost, 5432, databases, sslMode, caCert, setupCreds.Username, usePrivate)
	cfg := collector.BuildInstaclustr(creds.AgentID, creds.TenantID, comp, endpointsFor(creds, cmd))
	rendered, err := cfg.Render()
	if err != nil {
		rollbackRule()
		return err
	}
	configPath, _ := collector.ConfigPath()
	envPath, _ := collector.EnvPath()
	if err := collector.StoreSecrets(creds.AgentID, creds.Secret, monitorPassword); err != nil {
		rollbackRule()
		return err
	}
	if err := collector.WriteConfig(configPath, rendered); err != nil {
		rollbackRule()
		return err
	}
	if err := collector.WriteInstaclustrEnvFile(envPath, creds.Secret, monitorPassword, readOnlyKey); err != nil {
		rollbackRule()
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Wrote config: %s", configPath)))

	image, imageSource := resolveImage(cmd, creds)
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector image: %s (%s)", image, imageSource)))
	pinned, err := pinImage(image)
	if err != nil {
		rollbackRule()
		return fmt.Errorf("resolving collector image digest: %w", err)
	}
	image = pinned

	runner := collector.Runner{
		Name:        collector.DefaultContainerName,
		Image:       image,
		ConfigPath:  configPath,
		EnvFilePath: envPath,
		CACertPath:  caCert,
	}
	fmt.Println(style.Info(fmt.Sprintf("Starting collector container (%s)...", image)))
	if err := runContainer(runner); err != nil {
		fmt.Println(style.Warn("Container failed to start; rolling back the provisioned identity..."))
		if derr := client.DeleteCollector(creds.AgentID); derr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not auto-deprovision %s: %v (remove it from the console)", creds.AgentID, derr)))
		}
		collector.ClearSecrets(creds.AgentID)
		_ = os.Remove(configPath)
		_ = os.Remove(envPath)
		rollbackRule()
		return fmt.Errorf("%w\n\nRolled back. Fix Docker and re-run `dbg collector install`", err)
	}

	state := &collector.State{
		AgentID:              creds.AgentID,
		TenantID:             creds.TenantID,
		Domain:               creds.Domain,
		ContainerName:        runner.Name,
		Image:                image,
		ConfigPath:           configPath,
		EnvFilePath:          envPath,
		CACertPath:           caCert,
		TargetName:           ict.Name,
		InstaclustrClusterID: clusterID,
		InstaclustrUsername:  setupCreds.Username,
		CreatedAt:            time.Now().UTC(),
	}
	if created {
		state.FirewallRuleID = rule.ID
	}
	if err := collector.SaveState(state); err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Container started: %s", runner.Name)))

	if crashLooping(runner) {
		return errCollectorCrashLooping
	}
	verifyConnection(client, creds.AgentID, caCert)

	fmt.Println()
	fmt.Println("Collector installed. Next:")
	fmt.Println("  dbg collector status              # check connection")
	fmt.Println("  dbg collector logs -f             # watch it work")
	fmt.Println("  dbg collector refresh-firewall    # re-allowlist after an IP change")
	return nil
}

// dryRunInstaclustr previews the install with zero side effects: the only
// remote call already made is the read-only cluster GET. It prints what the
// real run would do — the firewall rule, the role SQL (with a placeholder
// password), the rendered config, and the container command.
func dryRunInstaclustr(cmd *cobra.Command, ict collector.InstaclustrTarget, seedHost string, databases []string, sslMode, apiUsername string, usePrivate bool, allowCIDR string) error {
	fmt.Println(style.Info("Dry run: nothing will be minted, written, allowlisted, or started."))
	fmt.Println()
	fmt.Printf("Would allowlist on the cluster firewall:  %s (POSTGRESQL)\n", allowCIDR)
	fmt.Printf("Would ensure role via the default user:   %s\n", collector.InstaclustrMonitorUser)
	fmt.Println("  CREATE ROLE " + collector.InstaclustrMonitorUser + " LOGIN PASSWORD '<generated>'")
	fmt.Println("  GRANT pg_monitor TO " + collector.InstaclustrMonitorUser)
	fmt.Println("  GRANT pg_read_all_data TO " + collector.InstaclustrMonitorUser)
	fmt.Println()
	comp := collector.BuildInstaclustrComponent(ict, seedHost, 5432, databases, sslMode, "", apiUsername, usePrivate)
	// Empty credentials: nothing is minted on a dry run, so the endpoints are
	// whatever the --*-url flags say (or the collector's production defaults).
	cfg := collector.BuildInstaclustr("<agent-id>", "<tenant-id>", comp, endpointsFor(&api.CollectorCredentials{}, cmd))
	rendered, err := cfg.Render()
	if err != nil {
		return err
	}
	fmt.Println("Would write collector.toml:")
	fmt.Println(rendered)
	image, imageSource := resolveImage(cmd, nil)
	runner := collector.Runner{
		Name:  collector.DefaultContainerName,
		Image: image,
	}
	fmt.Printf("Would run (%s): %s\n", imageSource, runner.RunCommandString())
	return nil
}

// ensureRoleWithRetry absorbs firewall-propagation latency: a rule created
// seconds ago may not pass packets yet, and the symptom is a dial timeout or
// refusal. Only connection failures retry — SQL failures are deterministic
// and retrying them just delays the real error.
func ensureRoleWithRetry(ctx context.Context, dsn, user, password string) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(8 * time.Second):
			}
		}
		if err = createInstaclustrRole(ctx, dsn, user, password); err == nil {
			return nil
		}
		if !collector.RetriableRoleError(err) {
			return err
		}
	}
	return err
}

// resolveInstaclustrCreds gathers the username + one API key: flag, env,
// caller-supplied default (state), then prompt (refused non-interactively,
// so CI fails with an instruction rather than hanging).
func resolveInstaclustrCreds(cmd *cobra.Command, defaultUser, keyFlag, keyEnv, keyLabel string) (collector.InstaclustrCreds, error) {
	user, _ := cmd.Flags().GetString("instaclustr-user")
	if user == "" {
		user = os.Getenv(instaclustrUserEnv)
	}
	if user == "" {
		user = defaultUser
	}
	if user == "" {
		if !interactiveTerminal() {
			return collector.InstaclustrCreds{}, fmt.Errorf("Instaclustr username required: pass --instaclustr-user or set %s", instaclustrUserEnv)
		}
		user = strings.TrimSpace(prompt("Instaclustr console username", ""))
		if user == "" {
			return collector.InstaclustrCreds{}, errors.New("aborted: no Instaclustr username given")
		}
	}
	key, err := resolveInstaclustrKey(cmd, keyFlag, keyEnv, keyLabel)
	if err != nil {
		return collector.InstaclustrCreds{}, err
	}
	return collector.InstaclustrCreds{Username: user, APIKey: key}, nil
}

func resolveInstaclustrKey(cmd *cobra.Command, flag, env, label string) (string, error) {
	key, _ := cmd.Flags().GetString(flag)
	if key == "" {
		key = os.Getenv(env)
	}
	if key == "" {
		if !interactiveTerminal() {
			return "", fmt.Errorf("%s required: pass --%s or set %s", label, flag, env)
		}
		key = strings.TrimSpace(promptPasswordOptional(label))
	}
	if key == "" {
		return "", fmt.Errorf("%s required: pass --%s or set %s", label, flag, env)
	}
	return key, nil
}

// --- refresh-firewall ------------------------------------------------------

var refreshFirewallCmd = &cobra.Command{
	Use:   "refresh-firewall",
	Short: "Re-allowlist the collector's current IP on the Instaclustr cluster firewall",
	Long: `When the collector's public IP changes (a new ISP lease, a redeploy without a
static egress), the Instaclustr cluster's firewall still allows the old one and
the collector's database connections time out. This re-detects the IP,
allowlists it, and removes the stale rule this CLI created earlier.`,
	RunE: runRefreshFirewall,
}

func runRefreshFirewall(cmd *cobra.Command, _ []string) error {
	st, err := requireState()
	if err != nil {
		return err
	}
	if st.InstaclustrClusterID == "" {
		return errors.New("the installed collector does not monitor an Instaclustr cluster. " +
			"refresh-firewall only applies to installs made with --provider instaclustr")
	}
	creds, err := resolveInstaclustrCreds(cmd, st.InstaclustrUsername, "instaclustr-api-key", instaclustrProvisioningEnv,
		"Instaclustr provisioning API key (setup only, never stored)")
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	allowRaw, _ := cmd.Flags().GetString("allow-ip")
	if allowRaw == "" {
		allowRaw, err = publicEgressIP(ctx)
		if err != nil {
			return err
		}
	}
	allowCIDR, err := collector.AllowCIDR(allowRaw)
	if err != nil {
		return err
	}
	rule, created, err := ensureFirewallRule(ctx, creds, st.InstaclustrClusterID, allowCIDR)
	if err != nil {
		return err
	}
	if created {
		fmt.Println(style.Success(fmt.Sprintf("✓ Allowlisted %s", allowCIDR)))
	} else {
		fmt.Println(style.Success(fmt.Sprintf("✓ %s already allowlisted", allowCIDR)))
	}
	// Retire the rule a previous run created for a different IP — but never
	// one this CLI did not create.
	if st.FirewallRuleID != "" && st.FirewallRuleID != rule.ID {
		if err := deleteFirewallRule(ctx, creds, st.FirewallRuleID); err != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not remove the stale firewall rule: %v (remove it from the console)", err)))
		} else {
			fmt.Println(style.Success("✓ Removed the stale rule from the previous IP"))
		}
	}
	if created {
		st.FirewallRuleID = rule.ID
		if err := collector.SaveState(st); err != nil {
			return err
		}
	}
	return nil
}
