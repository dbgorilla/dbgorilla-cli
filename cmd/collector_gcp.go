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
	// Method expressions (not wrapping closures) so the production defaults
	// carry no uncovered statement of their own.
	runGcpDeploy      = collector.GcpDeploy.Run
	runGcpDeployQuiet = collector.GcpDeploy.RunQuiet
)

func init() {
	installCmd.Flags().String("project", "", "GCP: project to act on (default: the credentials' project)")
	installCmd.Flags().String("deployment-name", collector.DefaultGcpDeploymentName, "GCP: Infrastructure Manager deployment name")
	installCmd.Flags().String("template-source", "", "GCP: deploy this Terraform template directory instead of the published one (must be a gs:// address)")
	installCmd.Flags().String("deploy-service-account", "", "GCP: service account Infrastructure Manager actuates Terraform as (projects/<p>/serviceAccounts/<email>)")
	installCmd.Flags().String("network", "", "GCP: VPC self-link for the collector instance (discovered from the database when omitted)")
}

// runInstallGCP deploys the collector to a Compute Engine managed instance
// group via Infrastructure Manager. It shares the aws target's provisioning
// spine — identity preflight, discovery, config-as-parameter, deploy with
// rollback — against Google's APIs.
func runInstallGCP(cmd *cobra.Command) error {
	apiURL, err := requireAPIURL(cmd)
	if err != nil {
		return err
	}
	if _, err := requireLogin(); err != nil {
		return err
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if st, _ := collector.LoadState(); st != nil && !dryRun {
		if !st.IsGCP() {
			return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
				st.AgentID)
		}
		status, err := gcpDeploymentStatus(st.Project, st.Region, st.DeploymentName)
		if err != nil {
			return err
		}
		if status != "" {
			// In-place component updates (the aws target's runUpdateAWS) need the
			// stored config read back off the deployment; that lands with the
			// update slice. Until then, changing the monitored set is
			// uninstall + install.
			return fmt.Errorf("collector deployment %q already exists (%s). "+
				"Run `dbg collector uninstall` first; in-place updates for the gcp target are not wired yet",
				st.DeploymentName, status)
		}
		fmt.Println(style.Warn(fmt.Sprintf(
			"⚠  deployment %q from the last install no longer exists — installing fresh.\n"+
				"   Collector %s is still provisioned in DBGorilla; remove it with `dbg collector uninstall` once this finishes.",
			st.DeploymentName, st.AgentID)))
	}

	// Preflight: the caller's own ADC; no keys pass through this tool.
	if err := gcpAvailable(); err != nil {
		return err
	}
	identity, err := gcpIdentity()
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ Google Cloud identity: %s", identity)))

	client := newAPIClient(cmd)
	supported, err := client.CollectorSupported()
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	if !supported {
		return api.ErrCollectorUnsupported
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

	deploymentName, _ := cmd.Flags().GetString("deployment-name")
	if err := resolveGcpAuth(cmd, &target, deploymentName, project); err != nil {
		return err
	}

	network, _ := cmd.Flags().GetString("network")
	if network == "" {
		network = target.Network
	}
	if network == "" {
		return errors.New("could not determine the collector's VPC (the database reports no private network); pass --network")
	}

	deployServiceAccount, _ := cmd.Flags().GetString("deploy-service-account")
	if deployServiceAccount == "" && !dryRun {
		return errors.New("pass --deploy-service-account: the account Infrastructure Manager actuates Terraform as " +
			"(it needs roles/config.agent plus permission to create the collector's instance group and service account)")
	}
	templateSource, _ := cmd.Flags().GetString("template-source")
	if templateSource == "" {
		templateSource = collector.HostedGcpTemplateSource()
	}
	commandsEnabled, _ := cmd.Flags().GetBool("enable-commands")
	dbPassword := awsDBPassword(cmd) // flag/env resolution is target-agnostic
	targets := []collector.GcpTarget{target}

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
		printGcpInputs(inputs)
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

	// Pin to a digest before it reaches the template: the MIG re-pulls when an
	// instance is recreated, so an unresolved tag changes version on a restart
	// nobody asked for — and upgrades would have nothing to act on.
	image, imageSource := resolveImage(cmd, creds)
	if pinned, perr := pinImageRemote(image); perr == nil {
		image = pinned
	} else {
		fmt.Println(style.Warn(fmt.Sprintf(
			"⚠  could not resolve %s to a fixed version (%v).\n"+
				"   Deploying the tag as-is: the collector may change version when its instance restarts.",
			image, perr)))
	}
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

	// Save state BEFORE the slow deploy so an interrupted install leaves a
	// tracked collector, not an orphaned deployment + identity.
	if serr := collector.SaveState(&collector.State{
		AgentID:        creds.AgentID,
		TenantID:       creds.TenantID,
		Domain:         creds.Domain,
		Target:         "gcp",
		Image:          image,
		TargetName:     target.InstanceID,
		Project:        project,
		Region:         target.Region,
		DeploymentName: deploymentName,
		CreatedAt:      time.Now().UTC(),
	}); serr != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not save local state: %v", serr)))
	}

	fmt.Printf("Deploying to Compute Engine (deployment %q)...\n", deploymentName)
	deploy := collector.GcpDeploy{
		Project: project, Region: target.Region, DeploymentName: deploymentName,
		TemplateSource: templateSource, ServiceAccount: deployServiceAccount,
		Inputs: inputs,
	}
	if err := deployGcp(deploy, "Deploying to Compute Engine…"); err != nil {
		if errors.Is(err, collector.ErrDeployTimeout) {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  Still deploying after %s. The deployment was NOT rolled back — "+
				"it is most likely still converging.", collector.GcpDeployTimeout())))
			fmt.Println("   Watch it with: dbg collector status")
			return nil
		}
		fmt.Println("Deploy failed; rolling back the provisioned identity and deployment...")
		if derr := client.DeleteCollector(creds.AgentID); derr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not auto-deprovision %s: %v (remove it from the console)", creds.AgentID, derr)))
		}
		if serr := deleteGcpDeployment(project, target.Region, deploymentName); serr != nil {
			fmt.Println(style.Warn(fmt.Sprintf("⚠  could not delete deployment %s: %v (delete it from the console)", deploymentName, serr)))
		}
		_ = collector.RemoveState()
		return fmt.Errorf("%w\n\nRolled back. Fix the issue above and re-run", err)
	}

	fmt.Println(style.Success(fmt.Sprintf("✓ Collector deploying to Compute Engine (deployment %s).", deploymentName)))
	printGcpGrantGuidance(target, deploymentName, project)
	return nil
}

// resolveGcpTarget picks and completes the database target. On an ambiguous
// project the typed error's candidate list is printed — never guessed from.
func resolveGcpTarget(cmd *cobra.Command, project string) (collector.GcpTarget, error) {
	id, _ := cmd.Flags().GetString("db-instance-id")
	providerType, _ := cmd.Flags().GetString("provider-type")
	seed := collector.GcpTarget{Project: project}
	if names, _ := cmd.Flags().GetString("db-name"); names != "" {
		seed.Databases = splitCSV(names)
	}
	return discoverGcpTarget(id, providerType, seed)
}

// resolveGcpAuth settles the target's auth: --db-password forces password
// auth; otherwise IAM when the database supports it (AlloyDB always does;
// Cloud SQL needs its flag on).
func resolveGcpAuth(cmd *cobra.Command, target *collector.GcpTarget, deploymentName, project string) error {
	if pw := awsDBPassword(cmd); pw != "" {
		target.AuthMethod = "password"
		if user, _ := cmd.Flags().GetString("db-user"); user != "" {
			target.User = user
		} else {
			target.User = "postgres"
		}
		return nil
	}
	if !target.IamEnabled {
		return fmt.Errorf("Cloud SQL instance %q does not have IAM database authentication enabled — "+
			"turn on the cloudsql.iam_authentication flag, or pass --db-password for password auth",
			target.InstanceID)
	}
	target.AuthMethod = "gcp_iam"
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	target.User = collector.GcpDatabaseUserFor(sa, target.Engine)
	return nil
}

// deployGcp runs the deployment with a spinner when interactive, mirroring
// deployStack.
func deployGcp(deploy collector.GcpDeploy, title string) error {
	if !interactiveTerminal() {
		return runGcpDeploy(deploy)
	}
	return withSpinner(title, func() error {
		_, err := runGcpDeployQuiet(deploy)
		return err
	})
}

// printGcpInputs prints the rendered template inputs with secrets redacted and
// the config decoded back to readable TOML.
func printGcpInputs(inputs map[string]string) {
	for _, k := range []string{"collector_image", "network", "region", "runtime_service_account"} {
		fmt.Printf("  %s: %s\n", k, inputs[k])
	}
	fmt.Println("  server_secret: (redacted)")
	if inputs["db_password"] != "" {
		fmt.Println("  db_password: (redacted)")
	}
	if decoded, err := collector.DecodeConfig(inputs["collector_config"]); err == nil {
		fmt.Printf("  collector_config (decoded):\n%s\n", decoded)
	}
}

// printGcpGrantGuidance names the two grant steps IAM auth needs: registering
// the collector's service account as a database user (an API call, not SQL),
// and the in-database read grants.
func printGcpGrantGuidance(target collector.GcpTarget, deploymentName, project string) {
	sa := collector.GcpRuntimeServiceAccountFor(deploymentName, project)
	if target.AuthMethod != "gcp_iam" {
		return
	}
	fmt.Println("\nGrant the collector database access:")
	if target.ProviderType == "alloydb" {
		fmt.Printf("  1. Register the service account as a database user:\n"+
			"     gcloud alloydb users create %s --cluster=%s --region=%s --type=IAM_BASED\n",
			sa, target.ClusterID, target.Region)
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
	fmt.Printf("Agent:      %s\n", st.AgentID)
	fmt.Printf("Tenant:     %s\n", st.TenantID)
	fmt.Printf("Target:     gcp — %s\n", st.TargetName)
	fmt.Printf("Image:      %s\n", st.Image)
	fmt.Printf("Deployment: %s\n", st.DeploymentName)
	status, err := gcpDeploymentStatus(st.Project, st.Region, st.DeploymentName)
	switch {
	case err != nil:
		fmt.Println(style.Warn(fmt.Sprintf("Deploy:     status unknown (%v)", err)))
	case status == "":
		fmt.Println(style.Warn("Deploy:     deployment not found"))
	default:
		fmt.Println(style.Success(fmt.Sprintf("Deploy:     %s", status)))
	}
	printConnectionStatus(cmd, st.AgentID)
	return nil
}
