package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	f.String("chart-ref", collector.DefaultChartRef, "Chart to install (a registry reference or a local path)")
	f.String("db-ca", collector.DBCACNPG, "Who signed the database's certificate: cnpg (its own CA) or system (a certificate your image already trusts)")
	f.String("agent-id", "", "Render for a collector identity you already have, instead of provisioning one (needs --tenant-id)")
	f.String("tenant-id", "", "Tenant the existing collector identity belongs to (needs --agent-id)")
	f.StringArray("set", nil, "Extra chart value as key=value; repeatable (e.g. --set image.tag=v1.2.3)")
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

	// Parsed here rather than at the point of use: everything that can reject the
	// operator's input has to run before an identity is minted, or a typo in a
	// chart value costs an orphaned collector server-side that they never see.
	rawSet, _ := cmd.Flags().GetStringArray("set")
	extra, err := collector.ParseExtraValues(rawSet)
	if err != nil {
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

	// An identity the caller already holds. Rendering a config for one is not a
	// special case: a collector outlives any single install, so re-creating a
	// deleted release, or managing one through GitOps where the identity was
	// provisioned separately, both need the config without minting anything.
	// Without this the only way to reuse an identity is to write the TOML by
	// hand, which is how a supported install path stops being the one people use.
	existing, err := existingIdentityFromFlags(cmd)
	if err != nil {
		return err
	}
	if existing != nil {
		agentID, tenantID = existing.agentID, existing.tenantID
		fmt.Println(style.Info(fmt.Sprintf("Using the collector identity you supplied (agent %s, tenant %s). Nothing is being provisioned.", agentID, tenantID)))
		// Said plainly because the omission is otherwise read as a failure. On the
		// provisioning path the server secret is printed once, and that is the only
		// time anyone sees it. Here the CLI never had it, which is the point -- it
		// stays wherever it was put when the identity was created.
		fmt.Println("Its server secret is not shown: this command never received one. Supply the")
		fmt.Println("value you already hold when you create the Secret below.")
		// On the provisioning path these three come back from the mint response.
		// Here there is no response, so an endpoint left empty is not a default --
		// it is a silent redirect. The collector treats empty as "use the built-in
		// production addresses", which are valid, resolvable, and belong to a
		// different deployment than the identity does. The result is a rejected
		// credential and an error that says nothing about the host it was sent to.
		if missing := missingEndpoints(eps); len(missing) > 0 {
			return fmt.Errorf(
				"an existing identity needs its endpoints too: %s not set. "+
					"They come from the provisioning response, and this command has no response to read. "+
					"Left empty the collector falls back to its built-in production addresses, so an "+
					"identity minted anywhere else is offered to the wrong deployment -- and the rejection "+
					"names neither the host nor the setting. Pass the URLs for the deployment you minted "+
					"against; if that is production, pass the production URLs explicitly",
				strings.Join(missing, ", "))
		}
	}

	if !dryRun && existing == nil {
		apiURL, err := requireInstallSession(cmd)
		if err != nil {
			return err
		}
		if err := requireNoInstall(); err != nil {
			return err
		}
		client, err := requireCollectorSupport(cmd, apiURL)
		if err != nil {
			return err
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
	chartRef, _ := cmd.Flags().GetString("chart-ref")
	install := collector.DefaultHelmInstall(configPath, secretName, collector.RBACFor(target.K8sMode, target.Namespace))
	install.Release, install.Namespace, install.Extra = release, relNS, extra
	// The SQL connection verifies the server unless the operator says otherwise,
	// and CNPG signs with a per-cluster CA that no OS trust store carries. The
	// mount is therefore part of the install, not an advanced option.
	install.MountClusterCA = target.NeedsClusterCA()
	if chartRef != "" {
		install.ChartRef = chartRef
	}

	if err := printHelmHandover(install, rendered, target); err != nil {
		return err
	}

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
func printHelmHandover(install collector.HelmInstall, rendered string, t collector.CNPGTarget) error {
	fmt.Println()
	fmt.Println(style.Info("--- collector.toml ---"))
	fmt.Print(rendered)

	// The namespace has to exist first. Both Secrets are created in it, and
	// `helm install --create-namespace` does not run until step 3 -- so following
	// these in order used to fail on the very first command with "namespaces
	// not found", which reads as a broken cluster rather than a step out of
	// sequence. Creating it here is harmless: --create-namespace tolerates a
	// namespace that already exists.
	fmt.Println(style.Info("--- 1. create the namespace and the Secret (run where you have cluster access) ---"))
	fmt.Printf("kubectl create namespace %s\n\n", install.Namespace)
	fmt.Println(install.SecretCommand(true))
	fmt.Println()
	if install.MountClusterCA {
		printClusterCAStep(install, t)
	}
	fmt.Println(style.Info("--- 3. install the collector ---"))
	fmt.Println(install.Command())
	fmt.Println()
	// --set-file reads the config from THIS machine. The step above tells the
	// reader to run these where they have cluster access, which is often a
	// different machine or a different person -- and there the path simply does
	// not exist. Helm's error names the missing file and not the reason, so the
	// reader concludes the config was never written. Point at the self-contained
	// form instead of leaving them to discover it below.
	fmt.Println("The command above reads the config from this machine. Installing from somewhere")
	fmt.Println("else means copying that file across, or using the self-contained values.yaml")
	fmt.Println("below, which carries the config inside it and needs nothing from here.")
	fmt.Println()
	fmt.Println(style.Info("--- or, for Argo CD / Flux: values.yaml ---"))
	values, err := collector.HelmValuesFragment(rendered, install.SecretRef, install.RBAC, install.Extra, install.ClusterCAYAML())
	if err != nil {
		return err
	}
	fmt.Print(values)

	fmt.Println()
	fmt.Printf("Component identity: %s  (stable across failover -- no instance in the key)\n", t.StableKey())
	fmt.Printf("SQL target:         %s:5432  (role service; CNPG's certificate does not cover pod names)\n", t.RoleServiceHost())

	if !t.MetricsOnly() {
		printPodMetricsPrerequisite(t.Namespace)
	}
	printDefaultQueriesPrerequisite(t)
	return nil
}

// printClusterCAStep prints the copy of the CNPG cluster CA into the collector's
// namespace.
//
// A step rather than a note: without it the collector fails its TLS handshake
// with "UnknownIssuer", which names a certificate and not a missing file, and
// so reads as a broken cluster rather than an incomplete install.
//
// The command extracts ca.crt on its own. The Secret CNPG publishes holds the
// CA private key beside it, and the obvious way to copy a Secret between
// namespaces takes both -- handing whoever runs the collector the ability to
// mint a certificate every client in that cluster will trust. That is a far
// larger grant than the read-only Role the rest of this design is careful
// about, and it would be an accident rather than a decision.
func printClusterCAStep(install collector.HelmInstall, t collector.CNPGTarget) {
	fmt.Println(style.Info("--- 2. copy the cluster's CA certificate to the collector's namespace ---"))
	fmt.Printf("Your connection uses ssl_mode %s, so the collector verifies the database's\n", sslModeOf(t))
	fmt.Println("certificate. CloudNativePG signs it with a CA that belongs to this cluster and")
	fmt.Println("is in no system trust store, so the collector needs a copy of it.")
	fmt.Println()
	fmt.Println(collector.ClusterCACopyCommand(t.Cluster, t.Namespace, install.Namespace))
	fmt.Println()
	fmt.Printf("This copies the certificate only. %s also holds the CA's PRIVATE KEY, and\n", collector.ClusterCASourceSecret(t.Cluster))
	fmt.Println("copying the whole Secret would put it wherever the collector runs -- enough to")
	fmt.Println("issue a certificate anything in that cluster would trust. Copy the one key.")
	fmt.Println()
	// A copy is a snapshot, and the original expires. CNPG rotates its CA on its
	// own schedule, and the copy does not follow -- so a collector that has run
	// for months stops connecting on a date nobody wrote down, with the same
	// UnknownIssuer error that means five other things.
	fmt.Println("The copy is a snapshot. CloudNativePG rotates this CA on its own schedule and")
	fmt.Println("the copy will not follow, so re-run the command above when it does. The expiry")
	fmt.Println("is on the cluster:")
	fmt.Println()
	fmt.Printf("  kubectl get cluster %s -n %s \\\n", t.Cluster, t.Namespace)
	fmt.Println("    -o jsonpath='{.status.certificates.expirations}'")
	fmt.Println()
	// The alternative shape, named here rather than in a manual, because this is
	// where someone is deciding whether the step applies to them.
	fmt.Println("If your cluster uses a certificate you supplied rather than CloudNativePG's own")
	fmt.Println("CA (spec.certificates.serverTLSSecret), none of this applies -- re-run with")
	fmt.Println("--db-ca system and no copy or mount is emitted.")
	fmt.Println()
}

// sslModeOf reports the effective ssl_mode, resolving the empty default so the
// printed reason matches the config the operator is holding.
func sslModeOf(t collector.CNPGTarget) string {
	if t.SSLMode == "" {
		return "verify-full"
	}
	return t.SSLMode
}

// printDefaultQueriesPrerequisite names the Cluster setting that turns off most
// of what the collector reads.
//
// CloudNativePG ships a default set of monitoring queries and enables it by
// default, which is why a stock cluster exports a useful set with no
// configuration. A cluster can switch that off, and then the metrics endpoint
// serves only the operator's own handful -- no replication lag, no database
// sizes, no connection counts.
//
// Printed in every mode, unlike the pod-metrics prerequisite. That one is about
// the cluster API, which metrics-only never consults; this one is about the
// database's own endpoint, which metrics-only depends on entirely. Grouping the
// two would repeat the mistake of assuming things lost together under one cause
// are lost together under another.
func printDefaultQueriesPrerequisite(t collector.CNPGTarget) {
	fmt.Println()
	fmt.Println(style.Info("--- and one setting on the cluster itself ---"))
	fmt.Println("CloudNativePG exports a standard set of database metrics unless the cluster")
	fmt.Println("turns them off. Confirm it has not:")
	fmt.Println()
	fmt.Printf("  kubectl get cluster %s -n %s \\\n", t.Cluster, t.Namespace)
	fmt.Println("    -o jsonpath='{.spec.monitoring.disableDefaultQueries}'")
	fmt.Println()
	fmt.Println("Empty or false is what you want. If it prints true, the collector installs and")
	fmt.Println("runs, and most of what it reports on will simply be absent -- replication lag,")
	fmt.Println("database sizes and connection counts among them.")
	fmt.Println()
	// Disk gets its own sentence because its absence is the one with a
	// consequence rather than a gap. Disk use is worked out from the database
	// sizes this setting removes, so the collector deliberately reports no
	// figure at all rather than one computed from what is left -- a partial
	// number would read far too low, and an alarm watching for a full volume
	// would stay confidently quiet while it filled. Nothing downstream can warn
	// about a measurement it never receives, so this is the only place the
	// person who set the field will ever be told.
	fmt.Println("Disk use depends on these queries too, and that one matters most: it is worked")
	fmt.Println("out from those database sizes, so no figure is reported at all rather than a")
	fmt.Println("wrong one. Nothing will warn you a volume is filling, and nothing will announce")
	fmt.Println("that it stopped watching.")
}

// printPodMetricsPrerequisite names the one cluster component the collector
// needs and does not install.
//
// CPU and memory come from the pod-metrics API, which is served by
// metrics-server. Managed clusters usually ship it as an addon, so it is easy
// to assume it is simply part of Kubernetes -- it is not, and on a self-managed
// or bare-metal cluster it may be absent altogether. Absent is not the same as
// empty: the API group does not exist, so a permission grant for it is valid,
// applies cleanly, and grants access to nothing.
//
// Deliberately says CPU and memory only. Disk is read from the volume records
// in the core API, which is present on every cluster, so metrics-server has no
// bearing on it. Sweeping all three into one sentence would send someone
// installing a component that was never the reason their disk panel is empty.
//
// That is why this is printed and not probed. This CLI does not contact the
// cluster (see CLAUDE.md), and the check that matters belongs to the collector,
// which runs there. What is useful here is telling the operator the
// prerequisite exists before they hand the commands to someone else -- it is
// one command to check and a long detour to diagnose later from a missing
// panel.
//
// The command suggested is `kubectl top pods`, not a read of the APIService
// registration, for two reasons. The registration can say the API is present
// while the server behind it is down -- it is an aggregated API, so "installed"
// and "working" are separate facts, and only one of them is what the operator
// needs. And `top` asks the same namespaced question the collector will ask, so
// it exercises the path rather than its paperwork.
func printPodMetricsPrerequisite(namespace string) {
	fmt.Println()
	fmt.Println(style.Info("--- before you install: one cluster prerequisite ---"))
	fmt.Println("CPU and memory readings come from the pod-metrics API, which metrics-server")
	fmt.Println("provides. Most managed clusters have it already; self-managed ones often do not.")
	fmt.Println("Check it is there AND working:")
	fmt.Println()
	fmt.Printf("  kubectl top pods -n %s\n", namespace)
	fmt.Println()
	fmt.Println("Columns of CPU and memory means you are set. An error instead means either")
	fmt.Println("metrics-server is not installed, or it is installed and not running -- the")
	fmt.Println("message says which, and they need different fixes.")
	fmt.Println()
	fmt.Println("Without it the collector still runs and still reports on the database itself.")
	fmt.Println("Only the CPU and memory figures are missing, and nothing will announce it.")
	fmt.Println()
	fmt.Println("Disk use does not depend on metrics-server: the volume's size is read from the")
	fmt.Println("main Kubernetes API, which every cluster has. (It does depend on the cluster")
	fmt.Println("setting below.)")
}

// missingEndpoints names the endpoint flags left unset, in the order the flags
// appear in help, so the error reads like the command the operator will type.
func missingEndpoints(eps collector.Endpoints) []string {
	var missing []string
	for _, e := range []struct {
		flag  string
		value string
	}{
		{"--auth-url", eps.AuthBaseURL},
		{"--otlp-url", eps.OtlpBaseURL},
		{"--opamp-url", eps.OpampBaseURL},
	} {
		if strings.TrimSpace(e.value) == "" {
			missing = append(missing, e.flag)
		}
	}
	return missing
}

// existingIdentity is a collector identity provisioned elsewhere.
//
// The server secret is deliberately not among these fields. The CLI does not
// need it: the config references it as an ${ENV} placeholder, and the operator
// puts the real value into the Kubernetes Secret themselves. Accepting it on a
// command line would put a credential into shell history to no purpose.
type existingIdentity struct {
	agentID  string
	tenantID string
}

// existingIdentityFromFlags returns nil when neither flag is set, which is the
// ordinary provisioning path.
//
// One flag without the other is refused rather than half-honoured. A config
// carrying a real agent id and an empty tenant renders, installs, and fails at
// the gateway with an authentication error that names neither -- and the first
// guess is always the secret, which is the one part that was correct.
func existingIdentityFromFlags(cmd *cobra.Command) (*existingIdentity, error) {
	agentID, _ := cmd.Flags().GetString("agent-id")
	tenantID, _ := cmd.Flags().GetString("tenant-id")
	agentID, tenantID = strings.TrimSpace(agentID), strings.TrimSpace(tenantID)
	switch {
	case agentID == "" && tenantID == "":
		return nil, nil
	case agentID == "":
		return nil, errors.New("--tenant-id needs --agent-id: both identify the collector, and one alone renders a config that authenticates as nobody")
	case tenantID == "":
		return nil, errors.New("--agent-id needs --tenant-id: both identify the collector, and one alone renders a config that authenticates as nobody")
	}
	return &existingIdentity{agentID: agentID, tenantID: tenantID}, nil
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
		DBCA:        get("db-ca"),
		K8sMode:     get("k8s-mode"),
		MetricsPort: port,
		MetricsTLS:  tls,
		MetricsCA:   get("metrics-ca"),
		CommandsOn:  commands,
	}, nil
}
