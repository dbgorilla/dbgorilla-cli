package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	stackOutput           = collector.StackOutput
)

func init() {
	installCmd.Flags().String("provider", "", "Database source: 'instaclustr' for a NetApp Instaclustr managed PostgreSQL cluster (default: the database named by --db-host)")
	installCmd.Flags().String("cluster-id", "", "Instaclustr cluster id (from the console URL or Cluster Details)")
	installCmd.Flags().String("instaclustr-user", "", "Instaclustr console username (or "+instaclustrUserEnv+")")
	installCmd.Flags().String("instaclustr-api-key", "", "Instaclustr provisioning API key, used for setup on this machine only (or "+instaclustrProvisioningEnv+")")
	installCmd.Flags().String("instaclustr-readonly-key", "", "Instaclustr READ-ONLY provisioning API key the collector keeps for discovery (or "+instaclustrReadOnlyEnv+")")
	installCmd.Flags().Bool("use-private-addresses", false, "Dial the cluster's private node addresses (VPC-peered collectors)")
	installCmd.Flags().String("allow-ip", "", "Public IP the firewall should allow for the collector (default: this machine's, auto-detected)")
	installCmd.Flags().Bool("stable-egress", true, "With --provider instaclustr --target aws: run the task behind a NAT gateway + Elastic IP so the firewall rule stays valid (adds ~USD 35-40/month plus NAT data processing)")
	installCmd.Flags().String("vpc-id", "", "VPC for the stable-egress private subnet (required with --stable-egress on aws)")
	installCmd.Flags().String("nat-subnet-cidr", "", "Unused CIDR in the VPC for the stable-egress private subnet, e.g. 10.0.200.0/28")

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

// runInstallInstaclustr is the install flow for the instaclustr source,
// dispatching on the deploy substrate: docker (below) or aws
// (runInstallInstaclustrAWS). gcp arrives with the GCP target.
func runInstallInstaclustr(cmd *cobra.Command) error {
	// Neither substrate can carry a cluster CA yet: the docker CA mount
	// replaces the container's system trust store (which the collector's own
	// control-plane TLS relies on), and the Fargate template has no CA-mount
	// mechanism at all. verify-full waits on a bundling story for both roots.
	if ca, _ := cmd.Flags().GetString("ca-cert"); ca != "" {
		return errors.New("--ca-cert is not supported with --provider instaclustr yet: neither the " +
			"docker CA mount (it replaces the system trust store the collector's own TLS needs) nor " +
			"the Fargate template can carry a cluster CA. The install uses ssl_mode=require " +
			"(encrypted, unverified) for now")
	}
	switch target, _ := cmd.Flags().GetString("target"); target {
	case "", "docker", "local":
	case "aws", "fargate":
		return runInstallInstaclustrAWS(cmd)
	default:
		return fmt.Errorf("unknown --target %q for --provider instaclustr (expected 'docker' or 'aws'; "+
			"'gcp' arrives with the GCP target)", target)
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

	in, err := resolveInstaclustrInstallInputs(cmd, apiURL, dryRun)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

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

	if dryRun {
		return dryRunInstaclustr(cmd, in.ict, in.seedHost, in.databases, in.sslMode, in.setupCreds.Username, in.usePrivate, allowCIDR)
	}

	// Firewall: the collector runs on THIS machine for the docker target, so
	// one rule covers both the setup connection and the collector. Every
	// failure below that follows a rule WE created rolls the rule back —
	// otherwise a later re-run finds it "pre-existing", never records
	// ownership, and refresh-firewall accumulates stale rules forever.
	rule, created, err := ensureFirewallRule(ctx, in.setupCreds, in.clusterID, allowCIDR)
	if err != nil {
		return err
	}
	rollbackRule := func() {
		if created {
			if derr := deleteFirewallRule(ctx, in.setupCreds, rule.ID); derr != nil {
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
	dsn := collector.InstaclustrAdminDSN(in.seedHost, 5432, in.ict.DefaultUserPassword)
	if err := ensureRoleWithRetry(ctx, dsn, collector.InstaclustrMonitorUser, monitorPassword); err != nil {
		rollbackRule()
		return fmt.Errorf("%w\n\nA just-created firewall rule can take ~a minute to apply; re-running is safe", err)
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Monitoring role %q ready (pg_monitor + pg_read_all_data)", collector.InstaclustrMonitorUser)))

	// Deep DB preflight with the monitoring role itself — the same gate the
	// local path runs, so a cluster missing pg_stat_statements is a warning
	// before anything is provisioned, not a silent gap after.
	monitorDSN := collector.InstaclustrAdminDSNAs(collector.InstaclustrMonitorUser, monitorPassword, in.seedHost, 5432)
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
	creds, err := in.client.ProvisionCollector()
	if err != nil {
		rollbackRule()
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))

	comp := collector.BuildInstaclustrComponent(in.ict, in.seedHost, 5432, in.databases, in.sslMode, caCert, in.setupCreds.Username, in.usePrivate, collector.DBPasswordEnv)
	cfg := collector.BuildInstaclustr(creds.AgentID, creds.TenantID, comp, endpointsFor(creds, cmd))
	rendered, err := cfg.Render()
	if err != nil {
		rollbackRule()
		return err
	}
	state := &collector.State{
		TargetName:           in.ict.Name,
		InstaclustrClusterID: in.clusterID,
		InstaclustrUsername:  in.setupCreds.Username,
	}
	if created {
		state.FirewallRuleID = rule.ID
	}
	return finishDockerInstall(cmd, in.client, creds, rendered, monitorPassword, caCert,
		func(envPath string) error {
			return collector.WriteInstaclustrEnvFile(envPath, creds.Secret, monitorPassword, in.readOnlyKey)
		},
		state, rollbackRule,
		"  dbg collector status              # check connection\n"+
			"  dbg collector logs -f             # watch it work\n"+
			"  dbg collector refresh-firewall    # re-allowlist after an IP change")
}

// instaclustrInstall is the front matter every instaclustr install resolves
// identically, whatever substrate runs the collector.
type instaclustrInstall struct {
	clusterID  string
	setupCreds collector.InstaclustrCreds
	// readOnlyKey stays empty on a dry run — nothing renders or ships it.
	readOnlyKey string
	client      *api.Client
	ict         collector.InstaclustrTarget
	seedHost    string
	usePrivate  bool
	sslMode     string
	databases   []string
}

// resolveInstaclustrInstallInputs gathers everything the substrates share:
// the cluster id, both API keys (by their two fates), the backend capability
// gate, and cluster discovery down to the seed host.
func resolveInstaclustrInstallInputs(cmd *cobra.Command, apiURL string, dryRun bool) (*instaclustrInstall, error) {
	clusterID, _ := cmd.Flags().GetString("cluster-id")
	if clusterID == "" {
		if !interactiveTerminal() {
			return nil, errors.New("--cluster-id is required with --provider instaclustr. " +
				"Find it in the Instaclustr console URL or Cluster Details")
		}
		clusterID = strings.TrimSpace(prompt("Instaclustr cluster id", ""))
		if clusterID == "" {
			return nil, errors.New("aborted: no cluster id given")
		}
	}
	setupCreds, err := resolveInstaclustrCreds(cmd, "", "instaclustr-api-key", instaclustrProvisioningEnv,
		"Instaclustr provisioning API key (setup only, never stored)")
	if err != nil {
		return nil, err
	}
	readOnlyKey := ""
	if !dryRun {
		if readOnlyKey, err = resolveInstaclustrKey(cmd, "instaclustr-readonly-key", instaclustrReadOnlyEnv,
			"Instaclustr READ-ONLY API key (the collector keeps this one)"); err != nil {
			return nil, err
		}
	}

	client, err := requireCollectorSupport(cmd, apiURL)
	if err != nil {
		return nil, err
	}

	// Discover the cluster through the Cluster Management API (read-only, so
	// the dry-run path shares it).
	ict, err := discoverInstaclustr(cmd.Context(), setupCreds, clusterID)
	if err != nil {
		return nil, err
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
		return nil, errors.New("no node has an address on the selected network side. " +
			"A private-network cluster needs --use-private-addresses; a public one must not set it")
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Cluster %q: %d node(s), PostgreSQL %s, %s %s",
		ict.Name, len(ict.Nodes), ict.PostgresVersion, ict.CloudProvider, ict.Region)))

	sslMode := ""
	if cmd.Flags().Changed("ssl-mode") {
		sslMode, _ = cmd.Flags().GetString("ssl-mode")
	}
	dbNames, _ := cmd.Flags().GetString("db-name")
	return &instaclustrInstall{
		clusterID:   clusterID,
		setupCreds:  setupCreds,
		readOnlyKey: readOnlyKey,
		client:      client,
		ict:         ict,
		seedHost:    seedHost,
		usePrivate:  usePrivate,
		sslMode:     sslMode,
		databases:   splitCSV(dbNames),
	}, nil
}

// setupMonitoringRole creates the monitoring role for a cloud install, via a
// temporary allowlist entry for THIS machine (the operator's IP is not the
// collector's). It hands back the rule, whether this run created it, and its
// remover — which every later exit path must call, unless the temporary rule
// turns out to be the collector's own.
func setupMonitoringRole(ctx context.Context, in *instaclustrInstall, monitorPassword string) (opRule collector.FirewallRule, opCreated bool, removeOperatorRule func(), err error) {
	operatorIP, err := publicEgressIP(ctx)
	if err != nil {
		return collector.FirewallRule{}, false, nil, err
	}
	operatorCIDR, err := collector.AllowCIDR(operatorIP)
	if err != nil {
		return collector.FirewallRule{}, false, nil, err
	}
	opRule, opCreated, err = ensureFirewallRule(ctx, in.setupCreds, in.clusterID, operatorCIDR)
	if err != nil {
		return collector.FirewallRule{}, false, nil, err
	}
	removeOperatorRule = func() {
		if opCreated {
			if derr := deleteFirewallRule(ctx, in.setupCreds, opRule.ID); derr != nil {
				fmt.Println(style.Warn(fmt.Sprintf("⚠  could not remove the temporary setup rule %s: %v (remove it from the console)", operatorCIDR, derr)))
			}
		}
	}
	dsn := collector.InstaclustrAdminDSN(in.seedHost, 5432, in.ict.DefaultUserPassword)
	if err := ensureRoleWithRetry(ctx, dsn, collector.InstaclustrMonitorUser, monitorPassword); err != nil {
		removeOperatorRule()
		return collector.FirewallRule{}, false, nil,
			fmt.Errorf("%w\n\nA just-created firewall rule can take ~a minute to apply; re-running is safe", err)
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Monitoring role %q ready (pg_monitor + pg_read_all_data)", collector.InstaclustrMonitorUser)))
	return opRule, opCreated, removeOperatorRule, nil
}

// allowlistCollectorEgress allowlists the collector's actual egress after a
// cloud deploy — the stable-egress address the deployment reserved (read via
// deployedEgressIP, whose error names the substrate) or the operator-supplied
// one — retires the operator's temporary rule unless it IS the collector's,
// and records rule ownership so refresh-firewall can retire it later.
func allowlistCollectorEgress(ctx context.Context, in *instaclustrInstall, stableEgress bool,
	deployedEgressIP func() (string, error), allowRaw string,
	opRule collector.FirewallRule, opCreated bool, removeOperatorRule func()) error {

	source := allowRaw
	if stableEgress {
		var err error
		if source, err = deployedEgressIP(); err != nil {
			removeOperatorRule()
			return err
		}
	}
	collectorCIDR, err := collector.AllowCIDR(source)
	if err != nil {
		removeOperatorRule()
		return err
	}
	rule, created, err := ensureFirewallRule(ctx, in.setupCreds, in.clusterID, collectorCIDR)
	if err != nil {
		removeOperatorRule()
		return fmt.Errorf("the collector deployed but its firewall entry failed: %w\n\n"+
			"Add %s to the cluster's PostgreSQL allowlist, or re-run `dbg collector refresh-firewall`", err, collectorCIDR)
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Firewall: allowlisted %s for the collector", collectorCIDR)))
	if opRule.ID != rule.ID {
		removeOperatorRule()
	} else if opCreated {
		// The operator's machine and the collector share an egress address, so
		// the temporary rule IS the collector's rule — this run created it and
		// must own it, or refresh-firewall could never retire it.
		created = true
	}

	if created {
		if st, lerr := collector.LoadState(); lerr == nil && st != nil {
			st.FirewallRuleID = rule.ID
			if serr := collector.SaveState(st); serr != nil {
				fmt.Println(style.Warn(fmt.Sprintf("⚠  could not record the firewall rule id: %v", serr)))
			}
		}
	}
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
	comp := collector.BuildInstaclustrComponent(ict, seedHost, 5432, databases, sslMode, "", apiUsername, usePrivate, collector.DBPasswordEnv)
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
		if st.IsAWS() {
			// The collector's egress is the stack's Elastic IP, not this
			// machine's address. A stack without the output was deployed
			// without stable egress — then only an explicit address makes
			// sense.
			allowRaw, err = stackOutput(st.StackName, st.Region, "EgressIP")
			if err != nil {
				return fmt.Errorf("%w\n\nPass --allow-ip explicitly for a deploy without stable egress", err)
			}
		} else {
			allowRaw, err = publicEgressIP(ctx)
			if err != nil {
				return err
			}
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
	// Record ownership honestly either way: the new rule's id when this run
	// created it, empty when the current rule pre-existed (the old owned id,
	// if any, was just retired above and must not linger in state).
	newID := ""
	if created {
		newID = rule.ID
	}
	if st.FirewallRuleID != newID {
		st.FirewallRuleID = newID
		if err := collector.SaveState(st); err != nil {
			return err
		}
	}
	return nil
}

// --- the aws substrate ------------------------------------------------------

// runInstallInstaclustrAWS deploys the collector for an Instaclustr cluster
// onto Fargate. Networking is explicit (--subnets/--security-group-id): there
// is no RDS instance to discover it from. By default the task runs behind a
// NAT gateway with an Elastic IP (--stable-egress), so the firewall rule
// created for it stays valid across every task restart; --stable-egress=false
// requires --allow-ip, because a plain Fargate task's public IP is ephemeral
// and unknowable in advance.
func runInstallInstaclustrAWS(cmd *cobra.Command) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}
	if st, _ := collector.LoadState(); st != nil && !dryRun {
		return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
			st.AgentID)
	}
	if err := awsAvailable(); err != nil {
		return err
	}
	identity, err := awsIdentity()
	if err != nil {
		return err
	}
	region := awsRegion()
	if region == "" {
		return errors.New("no AWS region resolved. Set AWS_REGION or configure a profile region")
	}
	accountID, err := awsAccountID()
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ AWS identity: %s (%s)", identity, region)))

	subnetsCSV, _ := cmd.Flags().GetString("subnets")
	subnets := splitCSV(subnetsCSV)
	sg, _ := cmd.Flags().GetString("security-group-id")
	if len(subnets) == 0 || sg == "" {
		return errors.New("--subnets and --security-group-id are required with --provider instaclustr --target aws: " +
			"there is no RDS instance to discover networking from. The security group needs egress to 443 " +
			"(the Instaclustr and DBGorilla APIs) and 5432 (the cluster)")
	}
	stableEgress, _ := cmd.Flags().GetBool("stable-egress")
	vpcID, _ := cmd.Flags().GetString("vpc-id")
	natCidr, _ := cmd.Flags().GetString("nat-subnet-cidr")
	allowRaw, _ := cmd.Flags().GetString("allow-ip")
	if stableEgress {
		if vpcID == "" || natCidr == "" {
			return errors.New("--vpc-id and --nat-subnet-cidr are required with --stable-egress " +
				"(the stack creates a private subnet routed through a NAT gateway with an Elastic IP; " +
				"the FIRST --subnets entry must be a public subnet for the NAT gateway). " +
				"Pass --stable-egress=false with --allow-ip to skip the NAT at the cost of firewall churn")
		}
		// Validate here rather than letting the deploy fail on it after the
		// role and firewall work is already done.
		if _, _, cerr := net.ParseCIDR(natCidr); cerr != nil {
			return fmt.Errorf("--nat-subnet-cidr %q is not a CIDR (e.g. 10.0.200.0/28)", natCidr)
		}
	} else if allowRaw == "" {
		return errors.New("--stable-egress=false needs --allow-ip: a plain Fargate task's public IP is " +
			"ephemeral, so the firewall entry must be an address you manage (a NAT you already have)")
	}

	in, err := resolveInstaclustrInstallInputs(cmd, apiURL, dryRun)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	stackName, _ := cmd.Flags().GetString("stack-name")
	templateURL, _ := cmd.Flags().GetString("template-url")

	// The monitor password is generated up front so both the role step and
	// the stack's DbPassword secret carry the same value.
	monitorPassword, err := collector.GenerateInstaclustrPassword()
	if err != nil {
		return err
	}
	assignIP, _ := cmd.Flags().GetString("assign-public-ip")
	if assignIP == "" {
		assignIP = "ENABLED"
	}
	comp := collector.BuildInstaclustrComponent(in.ict, in.seedHost, 5432, in.databases, in.sslMode, "",
		in.setupCreds.Username, in.usePrivate, collector.AwsDBPasswordEnv)
	input := collector.AwsStackInput{
		Region:          region,
		AccountID:       accountID,
		Components:      []collector.Component{comp},
		Subnets:         subnets,
		SecurityGroup:   sg,
		AssignPublicIP:  assignIP,
		CommandsEnabled: false,
		StableEgress:    stableEgress,
		VpcID:           vpcID,
		NatSubnetCidr:   natCidr,
	}

	if dryRun {
		input.AgentID, input.TenantID, input.Image = "<agent-id>", "<tenant-id>", "<image>"
		params, err := collector.AwsStackParams(input)
		if err != nil {
			return err
		}
		fmt.Printf("\nDry run — validating the template for stack %q (no identity minted, no firewall or role changes):\n", stackName)
		printAwsParams(params)
		return runFargateDeploy(collector.FargateDeploy{
			StackName: stackName, Params: params, DryRun: true, TemplateURL: templateURL,
		})
	}

	// Role first, through the operator's temporary firewall entry.
	opRule, opCreated, removeOperatorRule, err := setupMonitoringRole(ctx, in, monitorPassword)
	if err != nil {
		return err
	}

	fmt.Println(style.Info("Provisioning collector identity..."))
	creds, err := in.client.ProvisionCollector()
	if err != nil {
		removeOperatorRule()
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))

	image, imageSource := resolveImage(cmd, creds)
	image = pinImageOrWarn(image, "task")
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector image: %s (%s)", image, imageSource)))

	input.AgentID, input.TenantID, input.Image = creds.AgentID, creds.TenantID, image
	input.Endpoints = endpointsFor(creds, cmd)
	input.ServerSecret = creds.Secret
	input.DBPassword = monitorPassword
	input.InstaclustrKey = in.readOnlyKey
	params, err := collector.AwsStackParams(input)
	if err != nil {
		removeOperatorRule()
		return err
	}

	// Save state BEFORE the slow deploy (the aws pattern): an interrupted
	// install leaves a tracked collector, not an orphaned stack + identity.
	saveStateOrWarn(&collector.State{
		AgentID:              creds.AgentID,
		TenantID:             creds.TenantID,
		Domain:               creds.Domain,
		Target:               "aws",
		Image:                image,
		TargetName:           in.ict.Name,
		StackName:            stackName,
		Region:               region,
		InstaclustrClusterID: in.clusterID,
		InstaclustrUsername:  in.setupCreds.Username,
		CreatedAt:            time.Now().UTC(),
	})

	fmt.Printf("Deploying to Fargate (stack %q)...\n", stackName)
	if err := deployStack(collector.FargateDeploy{StackName: stackName, Params: params, TemplateURL: templateURL}, "Deploying to Fargate…"); err != nil {
		if errors.Is(err, collector.ErrDeployTimeout) {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  Still deploying after %s. The stack was NOT rolled back — "+
				"it is most likely still converging.", collector.DeployTimeout())))
			fmt.Printf("   Watch it with: dbg collector status, then re-run `dbg collector refresh-firewall` " +
				"once it is up (the EgressIP output appears only once the stack completes, so the firewall entry waits for it).\n")
			removeOperatorRule()
			return nil
		}
		fmt.Println("Deploy failed; rolling back the provisioned identity and stack...")
		if derr := in.client.DeleteCollector(creds.AgentID); derr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not auto-deprovision %s: %v (remove it from the console)", creds.AgentID, derr)))
		}
		if derr := deleteStack(stackName, region); derr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not delete stack %s: %v", stackName, derr)))
		}
		_ = collector.RemoveState()
		removeOperatorRule()
		return err
	}

	// Allowlist the collector's actual egress: the EIP the stack allocated
	// (stable egress) or the operator-supplied address.
	if err := allowlistCollectorEgress(ctx, in, stableEgress, func() (string, error) {
		eip, oerr := stackOutput(stackName, region, "EgressIP")
		if oerr != nil {
			return "", fmt.Errorf("the stack deployed but its EgressIP output could not be read: %w", oerr)
		}
		return eip, nil
	}, allowRaw, opRule, opCreated, removeOperatorRule); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Collector deployed. Next:")
	fmt.Println("  dbg collector status              # stack + connection")
	fmt.Println("  dbg collector logs -f             # CloudWatch logs")
	fmt.Println("  dbg collector refresh-firewall    # re-assert the allowlist entry")
	return nil
}
