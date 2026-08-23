package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	f := helmValuesCmd.Flags()
	f.String("name", "", "Display name for this database component (defaults to the cluster name)")
	f.String("namespace", "", "Kubernetes namespace the CNPG Cluster runs in (required)")
	f.String("cluster", "", "CNPG Cluster name (required)")
	f.String("db-name", "", "Comma-separated database names (empty = all databases on the server)")
	f.String("db-user", collector.DefaultDBUser, "Read-only database user the collector connects as")
	f.String("ssl-mode", "verify-full", "libpq ssl_mode for the SQL connection")
	f.String("k8s-mode", collector.K8sModeAuto, "Kubernetes API access: auto, enabled, or disabled (metrics-only)")
	f.Int("metrics-port", collector.DefaultMetricsPort, "CNPG instance-manager metrics port")
	f.Bool("metrics-tls", false, "The Cluster sets .spec.monitoring.tls.enabled (only needed with --k8s-mode disabled)")
	f.String("metrics-ca", "", "In-container path to the cluster CA bundle, required with --metrics-tls")
	f.String("release-name", collector.DefaultReleaseName, "Helm release name for the collector")
	f.String("release-namespace", collector.DefaultReleaseNamespace, "Namespace to install the collector into")
	f.String("secret-name", "dbg-collector-secrets", "Name of the Kubernetes Secret carrying the collector's credentials")
	f.Bool("enable-commands", false, "Allow the control plane to run query-analysis commands (execute_query, explain)")
	f.Bool("yes", false, "Skip confirmation prompts")
	f.Bool("dry-run", false, "Render everything without minting an identity or writing any file")
	f.String("auth-url", "", "Override the auth host base URL")
	f.String("otlp-url", "", "Override the OTLP gateway base URL")
	f.String("opamp-url", "", "Override the OpAMP websocket base URL")

	collectorCmd.AddCommand(helmValuesCmd)
}

var helmValuesCmd = &cobra.Command{
	Use:   "helm-values",
	Short: "Provision a collector for a CloudNativePG cluster and print its Helm values",
	Long: `Prepare an in-cluster collector for a CloudNativePG (CNPG) database.

Helm is the install path for Kubernetes, so this command does the two things Helm
cannot: it mints the collector's identity with DBGorilla, and it renders the
collector.toml for your cluster. It then prints the Secret and the ` + "`helm install`" + `
to run. It does not touch your cluster -- your platform team runs those two
commands, with their own credentials.

    dbg collector helm-values --namespace prod-db --cluster app-db

--cluster is required. One config entry monitors exactly one CNPG Cluster,
because the component's identity is fixed before discovery runs.

Kubernetes API access is optional. With it (the default, --k8s-mode auto) the
collector reads backup, WAL-archiving and failover state from the operator. Without
it, it still collects metrics -- see the warning the command prints.`,
	RunE: runHelmValues,
}

func runHelmValues(cmd *cobra.Command, _ []string) error {
	target, err := cnpgTargetFromFlags(cmd)
	if err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Warn BEFORE minting. Metrics-only is a supported mode and this is not a
	// blocker -- but it silently drops backup, recoverability and pod resource
	// collection, and nothing downstream ever says so: no error, no failed check,
	// and a dashboard that simply omits the sections it has no data for. The
	// person choosing the mode is the only one in a position to be told.
	if target.MetricsOnly() {
		fmt.Println(style.Warn("⚠  Metrics-only mode (--k8s-mode disabled): no Kubernetes API access, so this collector cannot collect:"))
		for _, c := range collector.DegradedCapabilities() {
			fmt.Printf("     - %s\n", c)
		}
		// The mode's own name invites the wrong conclusion. CPU and memory come
		// from the cluster's pod-metrics API, not from the database's own metrics
		// endpoint, so "metrics-only" collects database metrics and no pod ones.
		//
		// Disk is the one to state carefully. How full a volume is, is a ratio,
		// and only its top half comes from the database. The volume's size is a
		// cluster fact, so without the API there is a number and nothing to
		// divide it by -- which is why disk is lost here despite its main input
		// being on the very endpoint this mode does scrape.
		fmt.Println("   \"Metrics-only\" here means the database's own metrics endpoint. CPU and memory")
		fmt.Println("   come from the cluster API instead, so they are not included. Disk use needs the")
		fmt.Println("   volume's size, which is also a cluster fact, so it cannot be worked out either.")
		fmt.Println("   Granting the read-only Role later and re-running enables it -- no reinstall needed.")
		if !confirm(cmd, "Continue with metrics-only?") {
			return errors.New("aborted")
		}
	}

	agentID, tenantID := "<minted-on-run>", "<minted-on-run>"
	eps := endpointsFromFlags(cmd)

	if !dryRun {
		if _, err := requireAPIURL(cmd); err != nil {
			return err
		}
		if _, err := requireLogin(); err != nil {
			return err
		}
		if st, _ := collector.LoadState(); st != nil {
			return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
				st.AgentID)
		}
		client := newAPIClient(cmd)
		supported, err := client.CollectorSupported()
		if err != nil {
			return err
		}
		if !supported {
			return api.ErrCollectorUnsupported
		}
		fmt.Println(style.Info("Provisioning collector identity..."))
		creds, err := client.ProvisionCollector()
		if err != nil {
			return err
		}
		agentID, tenantID = creds.AgentID, creds.TenantID
		eps = endpointsFor(creds, cmd)
		fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))
		printServerSecret(creds.Secret)
	}

	cfg := collector.BuildCNPG(agentID, tenantID, target, eps)
	rendered, err := cfg.Render()
	if err != nil {
		return err
	}

	configPath, _ := collector.ConfigPath()
	if !dryRun {
		if err := collector.WriteConfig(configPath, rendered); err != nil {
			return err
		}
		fmt.Println(style.Success(fmt.Sprintf("✓ Wrote config: %s", configPath)))
	}

	release, _ := cmd.Flags().GetString("release-name")
	relNS, _ := cmd.Flags().GetString("release-namespace")
	secretName, _ := cmd.Flags().GetString("secret-name")
	install := collector.DefaultHelmInstall(configPath, secretName, collector.RBACFor(target.K8sMode, target.Namespace))
	install.Release, install.Namespace = release, relNS

	printHelmHandover(install, rendered, target)

	if dryRun {
		fmt.Println()
		fmt.Println(style.Info("DRY RUN — no identity was minted and no file was written."))
		return nil
	}

	if err := collector.SaveState(&collector.State{
		AgentID:          agentID,
		TenantID:         tenantID,
		Target:           "helm",
		TargetName:       target.StableKey(),
		ConfigPath:       configPath,
		ReleaseName:      install.Release,
		ReleaseNamespace: install.Namespace,
		DBNamespace:      target.Namespace,
		DBCluster:        target.Cluster,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not save local state: %v", err)))
	}

	fmt.Println()
	fmt.Println("Once the release is running:")
	fmt.Println("  dbg collector status     # check the control-plane connection")
	return nil
}

// printServerSecret shows the minted secret once, with the shell export that
// feeds it into the Secret command below. It is deliberately the only place the
// literal appears: the rendered config references it as ${DBG_SERVER_SECRET}, so
// nothing that gets pasted into a values file or a ConfigMap carries it.
func printServerSecret(secret string) {
	fmt.Println()
	fmt.Println(style.Warn("This is the only time the collector's secret is shown. Export it now:"))
	fmt.Printf("    export %s=%s\n", collector.SecretEnv, secret)
}

// printHelmHandover prints the three artifacts the operator needs, in the order
// they have to be applied: the config, the Secret, then the release.
func printHelmHandover(install collector.HelmInstall, rendered string, t collector.CNPGTarget) {
	fmt.Println()
	fmt.Println(style.Info("--- collector.toml ---"))
	fmt.Print(rendered)

	fmt.Println(style.Info("--- 1. create the Secret (run where you have cluster access) ---"))
	fmt.Println(install.SecretCommand(true))
	fmt.Println()
	fmt.Println(style.Info("--- 2. install the collector ---"))
	fmt.Println(install.Command())
	fmt.Println()
	fmt.Println(style.Info("--- or, for Argo CD / Flux: values.yaml ---"))
	fmt.Print(collector.HelmValuesFragment(rendered, install.SecretRef, install.RBAC))

	fmt.Println()
	fmt.Printf("Component identity: %s  (stable across failover -- no instance in the key)\n", t.StableKey())
	fmt.Printf("SQL target:         %s:5432  (role service; CNPG's certificate does not cover pod names)\n", t.RoleServiceHost())
}

// cnpgTargetFromFlags assembles the target from flags without validating it --
// Validate reports the first problem with the fix in the message.
func cnpgTargetFromFlags(cmd *cobra.Command) (collector.CNPGTarget, error) {
	get := func(n string) string { v, _ := cmd.Flags().GetString(n); return v }
	port, _ := cmd.Flags().GetInt("metrics-port")
	tls, _ := cmd.Flags().GetBool("metrics-tls")
	commands, _ := cmd.Flags().GetBool("enable-commands")
	return collector.CNPGTarget{
		Name:        get("name"),
		Namespace:   get("namespace"),
		Cluster:     get("cluster"),
		Databases:   splitCSV(get("db-name")),
		User:        get("db-user"),
		SSLMode:     get("ssl-mode"),
		K8sMode:     get("k8s-mode"),
		MetricsPort: port,
		MetricsTLS:  tls,
		MetricsCA:   get("metrics-ca"),
		CommandsOn:  commands,
	}, nil
}
