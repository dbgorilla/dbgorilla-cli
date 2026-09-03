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

// GCP-path seams: every operation that reaches a live Google Cloud project is
// substitutable, so the `--target gcp` workflow is testable without one.
var (
	gcpAvailable         = collector.GcpAvailable
	gcpIdentity          = collector.GcpIdentity
	gcpProject           = collector.GcpProject
	discoverGcpTarget    = collector.DiscoverGcpTarget
	resolveGcpSubnetwork = collector.ResolveGcpSubnetwork
	gcpDeploymentStatus  = collector.GcpDeploymentStatus
	deleteGcpDeployment  = collector.DeleteGcpDeployment
	scaleGcpMig          = collector.ScaleGcpMig
	restartGcpMig        = collector.RestartGcpMig
	tailGcpLogs          = collector.TailGcpLogs
	runGcpDeploy         = collector.GcpDeploy.Run
)

var (
	// gcpDeploymentNameRe is the service-account account_id grammar the
	// deployment name feeds.
	gcpDeploymentNameRe = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{4,28})[a-z0-9]$`)
	// gcpProjectNumberRe matches a project number, which cannot stand in for
	// the project ID in service-account emails.
	gcpProjectNumberRe = regexp.MustCompile(`^[0-9]+$`)
	// gcpServiceAccountRe is the resource form Infrastructure Manager expects.
	gcpServiceAccountRe = regexp.MustCompile(`^projects/[^/]+/serviceAccounts/[^/@]+@[^/]+$`)
)

// gcpSecretInputs are the template inputs a dry run must redact.
var gcpSecretInputs = []string{"server_secret", "db_password"}

// awsOnlyFlags are refused on the gcp target rather than silently ignored.
var awsOnlyFlags = []string{
	"dbi-resource-id", "subnets", "security-group-id", "assign-public-ip", "stack-name",
	"template-url", "config", "run-grant", "grant-user", "grant-password",
}

func init() {
	installCmd.Flags().String("project", "", "GCP: project ID to act on (default: the credentials' project, GOOGLE_CLOUD_PROJECT, or gcloud's active configuration)")
	installCmd.Flags().String("deployment-name", collector.DefaultGcpDeploymentName, "GCP: Infrastructure Manager deployment name")
	installCmd.Flags().String("template-source", "", "GCP: deploy this Terraform template directory instead of the published one (must be a gs:// address)")
	installCmd.Flags().String("deploy-service-account", "", "GCP: service account Infrastructure Manager actuates Terraform as (projects/<project>/serviceAccounts/<email>)")
	installCmd.Flags().String("network", "", "GCP: VPC for the collector instance, as projects/<project>/global/networks/<name> (discovered from the database when omitted). The VPC needs egress to the internet (Cloud NAT) for the image pull and the DBGorilla connection")
	installCmd.Flags().String("subnetwork", "", "GCP: subnetwork for the collector instance (auto-selected when the VPC has exactly one in the database's region; required otherwise)")
}

// runInstallGCP deploys the collector to a Compute Engine managed instance
// group via Infrastructure Manager.
func runInstallGCP(cmd *cobra.Command) error {
	apiURL, err := requireInstallSession(cmd)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Flag checks first: they cost nothing.
	for _, f := range awsOnlyFlags {
		if cmd.Flags().Changed(f) {
			return fmt.Errorf("--%s applies to --target aws only", f)
		}
	}
	deployServiceAccount, _ := cmd.Flags().GetString("deploy-service-account")
	switch {
	case deployServiceAccount == "" && !dryRun:
		return errors.New("pass --deploy-service-account: the account Infrastructure Manager actuates Terraform as " +
			"(it needs roles/config.agent plus permission to create the collector's instance group and service account)")
	case deployServiceAccount != "" && !gcpServiceAccountRe.MatchString(deployServiceAccount):
		return fmt.Errorf("--deploy-service-account %q must be of the form projects/<project>/serviceAccounts/<email>", deployServiceAccount)
	}
	deploymentName, _ := cmd.Flags().GetString("deployment-name")
	if !gcpDeploymentNameRe.MatchString(deploymentName) {
		return fmt.Errorf("--deployment-name %q must be 6-30 chars of [a-z0-9-], starting with a letter and not ending with '-' "+
			"(it names the collector's service account and secrets)", deploymentName)
	}
	providerType, _ := cmd.Flags().GetString("provider-type")
	if !collector.ValidGcpProviderType(providerType) {
		return fmt.Errorf("--provider-type %q is not a Google Cloud provider (expected cloud_sql or alloydb)", providerType)
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
		return fmt.Errorf("collector deployment %q already exists (%s). "+
			"Run `dbg collector uninstall` first; changing an installed gcp collector in place is not supported yet",
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
	if gcpProjectNumberRe.MatchString(project) {
		return fmt.Errorf("project %q is a project number; pass the project ID (--project), "+
			"which names the collector's service account", project)
	}
	target, err := resolveGcpTarget(cmd, project)
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Target database: %s (%s)", target.InstanceID, target.Host)))

	if err := requireNoRuntime(dryRun,
		func() (string, error) { return gcpDeploymentStatus(project, target.Region, deploymentName) },
		"deployment", deploymentName, "--deployment-name"); err != nil {
		return err
	}

	dbPassword := dbPasswordFlag(cmd)
	if err := resolveGcpAuth(&target, dbPassword, deploymentName, project); err != nil {
		return err
	}

	network, _ := cmd.Flags().GetString("network")
	if network == "" {
		network = target.Network
	}
	if network == "" {
		return errors.New("could not determine the collector's VPC (the database reports no private network); pass --network")
	}
	subnetwork, _ := cmd.Flags().GetString("subnetwork")
	if subnetwork == "" {
		if subnetwork, err = resolveGcpSubnetwork(network, target.Region); err != nil {
			return err
		}
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
			Subnetwork:      subnetwork,
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
		Subnetwork:      subnetwork,
		Region:          target.Region,
		DeploymentName:  deploymentName,
		Project:         project,
		ServerSecret:    creds.Secret,
		DBPassword:      dbPassword,
		CommandsEnabled: commandsEnabled,
	})
	if err != nil {
		deprovisionOrWarn(client, creds.AgentID)
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
		kept, derr := cloudDeployFailed(err, client, creds.AgentID, collector.GcpDeployTimeout(), "deployment", deploymentName,
			func() error {
				return withSpinner("Deleting the deployment…", func() error {
					return deleteGcpDeployment(project, target.Region, deploymentName)
				})
			},
			"   Watch it with: dbg collector status\n")
		if kept {
			printGcpGrantGuidance(target, deploymentName, project)
		}
		return derr
	}

	fmt.Println(style.Success(fmt.Sprintf("✓ Collector deploying to Compute Engine (deployment %s).", deploymentName)))
	printGcpGrantGuidance(target, deploymentName, project)
	fmt.Println("\nConfirm it connected with: dbg collector status")
	return nil
}

// resolveGcpTarget picks and completes the database target. An ambiguous
// project becomes a picker on a real terminal; otherwise the candidates are
// listed in the error.
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
		choice, perr := pickTarget(amb, "")
		if perr != nil {
			return collector.GcpTarget{}, perr
		}
		return discoverGcpTarget(choice.ID, choice.ProviderType, seed)
	}
	return target, err
}

// resolveGcpAuth settles the target's auth: --db-password forces password
// auth; otherwise IAM as the runtime service account's database identity.
func resolveGcpAuth(target *collector.GcpTarget, dbPassword, deploymentName, project string) error {
	if dbPassword != "" {
		target.AuthMethod = "password"
		return nil
	}
	if target.Engine == "mysql" && target.ProviderType == "cloud_sql" {
		return fmt.Errorf("the collector does not support IAM database authentication for "+
			"Cloud SQL MySQL yet — pass --db-password to use password auth for %q",
			target.InstanceID)
	}
	if !target.IamEnabled {
		return fmt.Errorf("Cloud SQL instance %q does not have IAM database authentication enabled — "+
			"turn on the cloudsql.iam_authentication flag, or pass --db-password for password auth",
			target.InstanceID)
	}
	if target.User != "" {
		fmt.Println(style.Warn("⚠  --db-user only applies to password auth — IAM auth derives the " +
			"database user from the runtime service account; ignoring it"))
	}
	target.AuthMethod = "gcp_iam"
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	target.User = collector.GcpDatabaseUserFor(sa, target.Engine)
	return nil
}

// printGcpGrantGuidance names the two grant steps IAM auth needs: registering
// the collector's service account as a database user, and the in-database
// read grants.
func printGcpGrantGuidance(target collector.GcpTarget, deploymentName, project string) {
	if target.AuthMethod != "gcp_iam" {
		return
	}
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	fmt.Println("\nGrant the collector database access:")
	if target.ProviderType == "alloydb" {
		// AlloyDB registers the literal username given; the collector logs in
		// as the trimmed form.
		fmt.Printf("  1. Register the service account as a database user:\n"+
			"     gcloud alloydb users create %s --cluster=%s --region=%s --type=IAM_BASED\n",
			target.User, target.ClusterID, target.Region)
	} else {
		fmt.Printf("  1. Register the service account as a database user:\n"+
			"     gcloud sql users create %s --instance=%s --type=cloud_iam_service_account\n",
			sa, target.InstanceID)
	}
	fmt.Printf("  2. Connect as an admin and grant read access to %q:\n", target.User)
	for _, stmt := range collector.GcpGrantStatements(target.User, target.Databases) {
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
