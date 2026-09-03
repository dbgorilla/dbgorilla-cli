package cmd

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// GCP-path seams: same purpose as the AWS block in collector.go — every
// operation that reaches a live Google Cloud project is substitutable, so the
// whole `--target gcp` workflow is exercisable without a project.
var (
	gcpAvailable        = collector.GcpAvailable
	gcpIdentity         = collector.GcpIdentity
	gcpProject          = collector.GcpProject
	discoverGcpTarget   = collector.DiscoverGcpTarget
	gcpDeploymentStatus = collector.GcpDeploymentStatus
	deleteGcpDeployment = collector.DeleteGcpDeployment
	scaleGcpMig         = collector.ScaleGcpMig
	restartGcpMig       = collector.RestartGcpMig
	tailGcpLogs         = collector.TailGcpLogs
	// A method expression (not a wrapping closure) so the production default
	// carries no uncovered statement of its own.
	runGcpDeploy = collector.GcpDeploy.Run
)

// gcpDeploymentNameRe is the SA account_id contract the deployment name feeds
// (6-30 chars, lowercase letter first, [a-z0-9-], no trailing dash).
var gcpDeploymentNameRe = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{4,28})[a-z0-9]$`)

// gcpSecretInputs are the template inputs a dry run must redact.
var gcpSecretInputs = []string{"server_secret", "db_password"}

func init() {
	installCmd.Flags().String("project", "", "GCP: project to act on (default: the credentials' project)")
	installCmd.Flags().String("deployment-name", collector.DefaultGcpDeploymentName, "GCP: Infrastructure Manager deployment name")
	installCmd.Flags().String("template-source", "", "GCP: deploy this Terraform template directory instead of the published one (must be a gs:// address)")
	installCmd.Flags().String("deploy-service-account", "", "GCP: service account Infrastructure Manager actuates Terraform as (projects/<p>/serviceAccounts/<email>)")
	installCmd.Flags().String("network", "", "GCP: VPC self-link for the collector instance (discovered from the database when omitted)")
}

// runInstallGCP deploys the collector to a Compute Engine managed instance
// group via Infrastructure Manager, on the shared cloud install spine.
func runInstallGCP(cmd *cobra.Command) error {
	apiURL, err := requireInstallSession(cmd)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Flag checks first: they cost nothing, and failing them after discovery
	// has made a round of API calls wastes the operator's time.
	deployServiceAccount, _ := cmd.Flags().GetString("deploy-service-account")
	if deployServiceAccount == "" && !dryRun {
		return errors.New("pass --deploy-service-account: the account Infrastructure Manager actuates Terraform as " +
			"(it needs roles/config.agent plus permission to create the collector's instance group and service account)")
	}
	deploymentName, _ := cmd.Flags().GetString("deployment-name")
	// The deployment name becomes the runtime service account's account_id and
	// the secret-id prefix, so it inherits the SA naming contract. Rejecting a
	// bad name here costs nothing; discovering it inside Terraform costs the
	// operator a minted identity and twenty minutes.
	if !gcpDeploymentNameRe.MatchString(deploymentName) {
		return fmt.Errorf("--deployment-name %q must be 6-30 chars of [a-z0-9-], starting with a letter and not ending with '-' "+
			"(it names the collector's service account and secrets)", deploymentName)
	}
	templateSource, _ := cmd.Flags().GetString("template-source")
	if templateSource == "" {
		templateSource = collector.HostedGcpTemplateSource()
	}

	prior, status, err := priorCloudInstall(dryRun, (*collector.State).IsGCP,
		func(st *collector.State) (string, error) {
			return gcpDeploymentStatus(st.Project, st.Region, st.DeploymentName)
		},
		"deployment", func(st *collector.State) string { return st.DeploymentName })
	if err != nil {
		return err
	}
	if prior != nil {
		// In-place component updates (the aws target's runUpdateAWS) need the
		// stored config read back off the deployment; until that lands,
		// changing the monitored set is uninstall + install.
		return fmt.Errorf("collector deployment %q already exists (%s). "+
			"Run `dbg collector uninstall` first; in-place updates for the gcp target are not wired yet",
			prior.DeploymentName, status)
	}

	if err := printCloudIdentity("Google Cloud", gcpAvailable, gcpIdentity); err != nil {
		return err
	}
	client, err := requireCollectorSupport(cmd, apiURL)
	if err != nil {
		return err
	}

	project, _ := cmd.Flags().GetString("project")
	if project == "" {
		if project, err = gcpProject(); err != nil {
			return err
		}
	}
	target, err := resolveGcpTarget(cmd, project)
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Target database: %s (%s)", target.InstanceID, target.Host)))

	dbPassword := dbPasswordFlag(cmd)
	if err := resolveGcpAuth(&target, dbPassword, deploymentName, project); err != nil {
		return err
	}

	// The collector instance joins the database's VPC unless --network says
	// otherwise; a database without a private network has nothing to join.
	network, _ := cmd.Flags().GetString("network")
	if network == "" {
		network = target.Network
	}
	if network == "" {
		return errors.New("could not determine the collector's VPC (the database reports no private network); pass --network")
	}

	targets := []collector.GcpTarget{target}
	commandsEnabled := resolveCommands(cmd, targets, gcpTargetLabel)

	if dryRun {
		image, _ := resolveImage(cmd, nil)
		inputs, err := collector.GcpDeployInputs(collector.GcpStackInput{
			AgentID: "DRY-RUN", TenantID: "DRY-RUN",
			Image:           image,
			Targets:         targets,
			Network:         network,
			Region:          target.Region,
			DeploymentName:  deploymentName,
			Project:         project,
			DBPassword:      dbPassword,
			CommandsEnabled: commandsEnabled,
		})
		if err != nil {
			return err
		}
		fmt.Printf("\nDry run — probing the template for deployment %q (no identity minted):\n", deploymentName)
		printDeployParams(inputs, gcpSecretInputs, "collector_config")
		return runGcpDeploy(collector.GcpDeploy{
			Project: project, Region: target.Region, DeploymentName: deploymentName,
			TemplateSource: templateSource, DryRun: true,
		})
	}

	fmt.Println("Provisioning collector identity...")
	creds, err := client.ProvisionCollector()
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector provisioned (agent %s, tenant %s)", creds.AgentID, creds.TenantID)))

	image, imageSource := resolveImage(cmd, creds)
	image = pinImageOrWarn(image, "instance")
	fmt.Println(style.Success(fmt.Sprintf("✓ Collector image: %s (%s)", image, imageSource)))

	inputs, err := collector.GcpDeployInputs(collector.GcpStackInput{
		AgentID:         creds.AgentID,
		TenantID:        creds.TenantID,
		Image:           image,
		Endpoints:       endpointsFor(creds, cmd),
		Targets:         targets,
		Network:         network,
		Region:          target.Region,
		DeploymentName:  deploymentName,
		Project:         project,
		ServerSecret:    creds.Secret,
		DBPassword:      dbPassword,
		CommandsEnabled: commandsEnabled,
	})
	if err != nil {
		return err
	}

	saveStateOrWarn(&collector.State{
		AgentID:        creds.AgentID,
		TenantID:       creds.TenantID,
		Domain:         creds.Domain,
		Target:         "gcp",
		Image:          image,
		TargetName:     target.DisplayName(),
		Project:        project,
		Region:         target.Region,
		DeploymentName: deploymentName,
		CreatedAt:      time.Now().UTC(),
	})

	fmt.Printf("Deploying to Compute Engine (deployment %q)...\n", deploymentName)
	deploy := collector.GcpDeploy{
		Project: project, Region: target.Region, DeploymentName: deploymentName,
		TemplateSource: templateSource, ServiceAccount: deployServiceAccount,
		Inputs: inputs,
	}
	if err := withSpinner("Deploying to Compute Engine…", func() error { return runGcpDeploy(deploy) }); err != nil {
		return cloudDeployFailed(err, client, creds.AgentID, collector.GcpDeployTimeout(), "deployment", deploymentName,
			func() error { return deleteGcpDeployment(project, target.Region, deploymentName) },
			"   Watch it with: dbg collector status\n")
	}

	fmt.Println(style.Success(fmt.Sprintf("✓ Collector deploying to Compute Engine (deployment %s).", deploymentName)))
	printGcpGrantGuidance(target, deploymentName, project)
	return nil
}

// resolveGcpTarget picks and completes the database target. An ambiguous
// project becomes a picker on a real terminal; otherwise the typed error's
// candidate list is surfaced — never guessed from.
func resolveGcpTarget(cmd *cobra.Command, project string) (collector.GcpTarget, error) {
	id, _ := cmd.Flags().GetString("db-instance-id")
	providerType, _ := cmd.Flags().GetString("provider-type")
	seed := collector.GcpTarget{Project: project}
	if names, _ := cmd.Flags().GetString("db-name"); names != "" {
		seed.Databases = splitCSV(names)
	}
	seed.User, _ = cmd.Flags().GetString("db-user")

	target, err := discoverGcpTarget(id, providerType, seed)
	var amb *collector.AmbiguousTargetError
	if errors.As(err, &amb) && interactiveSelectable(cmd) {
		choice, perr := pickTarget(amb)
		if perr != nil {
			return collector.GcpTarget{}, perr
		}
		return discoverGcpTarget(choice.ID, choice.ProviderType, seed)
	}
	return target, err
}

// resolveGcpAuth settles the target's auth: --db-password forces password
// auth (as the --db-user, else the collector's default user); otherwise IAM
// when the database supports it (AlloyDB always does; Cloud SQL needs its
// flag on), as the runtime service account's database identity.
func resolveGcpAuth(target *collector.GcpTarget, dbPassword, deploymentName, project string) error {
	if dbPassword != "" {
		target.AuthMethod = "password"
		return nil
	}
	if !target.IamEnabled {
		return fmt.Errorf("Cloud SQL instance %q does not have IAM database authentication enabled — "+
			"turn on the cloudsql.iam_authentication flag, or pass --db-password for password auth",
			target.InstanceID)
	}
	// The collector's support matrix deliberately excludes mysql + cloud_sql +
	// gcp_iam until its MySQL dial is live-proven against the PSA endpoint;
	// rendering that combination installs a collector that refuses its own
	// config at startup. Refuse here instead, where the operator can act.
	if target.Engine == "mysql" && target.ProviderType == "cloud_sql" {
		return fmt.Errorf("the collector does not support IAM database authentication for "+
			"Cloud SQL MySQL yet — pass --db-password to use password auth for %q",
			target.InstanceID)
	}
	target.AuthMethod = "gcp_iam"
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	target.User = collector.GcpDatabaseUserFor(sa, target.Engine)
	return nil
}

// printGcpGrantGuidance names the two grant steps IAM auth needs: registering
// the collector's service account as a database user (an API call, not SQL),
// and the in-database read grants.
func printGcpGrantGuidance(target collector.GcpTarget, deploymentName, project string) {
	if target.AuthMethod != "gcp_iam" {
		return
	}
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	fmt.Println("\nGrant the collector database access:")
	if target.ProviderType == "alloydb" {
		// AlloyDB registers the LITERAL username given (unlike Cloud SQL, whose
		// API strips the .gserviceaccount.com suffix server-side), and the
		// collector logs in as the trimmed form — so the trimmed form is what
		// must be registered, or step 2's grants land on a user that never
		// matches the login.
		fmt.Printf("  1. Register the service account as a database user:\n"+
			"     gcloud alloydb users create %s --cluster=%s --region=%s --type=IAM_BASED\n",
			target.User, target.ClusterID, target.Region)
	} else {
		fmt.Printf("  1. Register the service account as a database user:\n"+
			"     gcloud sql users create %s --instance=%s --type=cloud_iam_service_account\n",
			sa, target.InstanceID)
	}
	fmt.Printf("  2. Connect as an admin and grant read access to %q:\n", target.User)
	for _, stmt := range collector.GrantStatements(target.User, target.Databases) {
		fmt.Printf("     %s\n", stmt)
	}
}

// gcpStatus reports a GCP-deployed collector's deployment state plus its
// control-plane connection.
func gcpStatus(cmd *cobra.Command, st *collector.State) error {
	return cloudStatus(cmd, st, "Deployment", st.DeploymentName, func() (string, error) {
		return gcpDeploymentStatus(st.Project, st.Region, st.DeploymentName)
	})
}
