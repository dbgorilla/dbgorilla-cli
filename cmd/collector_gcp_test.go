package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// The `--target gcp` install mints an identity, creates an Infrastructure
// Manager deployment, and on failure rolls both back. These drive it end to
// end with every Google Cloud call faked through the seams.

// --- seams -----------------------------------------------------------------

func stubGCPOK(t *testing.T) {
	t.Helper()
	stubGcpAvailable(t, nil)
	stubGcpIdentity(t, "dev@example.com", nil)
	stubGcpProject(t, "acme-prod", nil)
	stubRemoteDigest(t, nil)
}

func stubGcpAvailable(t *testing.T, err error) *int {
	t.Helper()
	calls := new(int)
	orig := gcpAvailable
	gcpAvailable = func() error { *calls++; return err }
	t.Cleanup(func() { gcpAvailable = orig })
	return calls
}

func stubGcpIdentity(t *testing.T, email string, err error) {
	t.Helper()
	orig := gcpIdentity
	gcpIdentity = func() (string, error) { return email, err }
	t.Cleanup(func() { gcpIdentity = orig })
}

func stubGcpProject(t *testing.T, project string, err error) {
	t.Helper()
	orig := gcpProject
	gcpProject = func() (string, error) { return project, err }
	t.Cleanup(func() { gcpProject = orig })
}

// stubGcpDiscover answers discovery with target; explicit seed fields still
// win, mirroring the real merge.
func stubGcpDiscover(t *testing.T, target collector.GcpTarget, err error) {
	t.Helper()
	orig := discoverGcpTarget
	discoverGcpTarget = func(id, provider string, into collector.GcpTarget) (collector.GcpTarget, error) {
		if err != nil {
			return into, err
		}
		out := target
		out.Project = into.Project
		if id != "" {
			out.InstanceID = id
		}
		if provider != "" {
			out.ProviderType = provider
		}
		if len(into.Databases) > 0 {
			out.Databases = into.Databases
		}
		if into.User != "" {
			out.User = into.User
		}
		return out, nil
	}
	t.Cleanup(func() { discoverGcpTarget = orig })
}

func stubGcpDeploymentStatus(t *testing.T, status string, err error) {
	t.Helper()
	orig := gcpDeploymentStatus
	gcpDeploymentStatus = func(string, string, string) (string, error) { return status, err }
	t.Cleanup(func() { gcpDeploymentStatus = orig })
}

type gcpDeployCall struct {
	count  int
	deploy collector.GcpDeploy
}

func stubGcpDeploy(t *testing.T, err error) *gcpDeployCall {
	t.Helper()
	rec := &gcpDeployCall{}
	orig := runGcpDeploy
	runGcpDeploy = func(d collector.GcpDeploy) error {
		rec.count++
		rec.deploy = d
		return err
	}
	t.Cleanup(func() { runGcpDeploy = orig })
	return rec
}

func stubDeleteGcpDeployment(t *testing.T, err error) *bool {
	t.Helper()
	called := new(bool)
	orig := deleteGcpDeployment
	deleteGcpDeployment = func(string, string, string) error { *called = true; return err }
	t.Cleanup(func() { deleteGcpDeployment = orig })
	return called
}

// gcpCmd builds a command carrying the flags the GCP install path reads.
func gcpCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := baseCmd()
	c.Flags().String("target", "gcp", "")
	c.Flags().String("db-instance-id", "", "")
	c.Flags().String("provider-type", "", "")
	c.Flags().String("db-name", "", "")
	c.Flags().String("db-user", "", "")
	c.Flags().String("db-password", "", "")
	c.Flags().String("image", collector.DefaultImage, "")
	c.Flags().String("commands", "", "")
	c.Flags().Bool("enable-commands", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().String("auth-url", "", "")
	c.Flags().String("otlp-url", "", "")
	c.Flags().String("project", "", "")
	c.Flags().String("deployment-name", "dbg-test", "")
	c.Flags().String("template-source", "", "")
	c.Flags().String("deploy-service-account", "projects/acme-prod/serviceAccounts/deployer@acme-prod.iam.gserviceaccount.com", "")
	c.Flags().String("network", "", "")
	c.SetContext(context.Background())
	return c
}

// completeGcpTarget is a fully-discovered Cloud SQL Postgres instance with IAM
// auth on, so tests that are not about discovery can skip it.
func completeGcpTarget() collector.GcpTarget {
	return collector.GcpTarget{
		ProviderType: "cloud_sql",
		Project:      "acme-prod",
		Region:       "us-central1",
		InstanceID:   "prod-pg",
		Engine:       "postgres",
		Host:         "abc.us-central1.sql-psa.goog.",
		Port:         5432,
		ServerCaMode: "GOOGLE_MANAGED_CAS_CA",
		IamEnabled:   true,
		Network:      "projects/acme-prod/global/networks/default",
	}
}

// decodedConfig returns the collector.toml a deploy carried.
func decodedConfig(t *testing.T, d collector.GcpDeploy) string {
	t.Helper()
	cfg, err := collector.DecodeConfig(d.Inputs["collector_config"])
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

// --- install ---------------------------------------------------------------

func TestRunInstallGCP_HappyPath(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubGCPOK(t)
	stubGcpDiscover(t, completeGcpTarget(), nil)
	deploys := stubGcpDeploy(t, nil)
	srv := installServer(t, "agent-gcp")
	defer srv.Close()

	c := gcpCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallGCP(c) })
	if err != nil {
		t.Fatalf("runInstallGCP: %v\n%s", err, out)
	}
	if deploys.count != 1 || deploys.deploy.DryRun {
		t.Fatalf("want exactly one real deploy, got %+v", deploys)
	}
	d := deploys.deploy
	if d.Project != "acme-prod" || d.Region != "us-central1" || d.DeploymentName != "dbg-test" {
		t.Errorf("deployment addressed wrongly: %+v", d)
	}
	if d.ServiceAccount == "" || d.TemplateSource != collector.HostedGcpTemplateSource() {
		t.Errorf("deploy must carry the actuating account and the published template, got %+v", d)
	}
	// The IAM database user is the runtime service account the template will
	// create, by naming contract — rendered before that account exists.
	if d.Inputs["runtime_service_account"] != "dbg-test@acme-prod.iam.gserviceaccount.com" {
		t.Errorf("runtime SA = %q", d.Inputs["runtime_service_account"])
	}
	cfg := decodedConfig(t, d)
	for _, want := range []string{`method = "gcp_iam"`, `user = "dbg-test@acme-prod.iam"`, `ssl_mode = "verify-full"`, `enabled = true`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %s:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "sek") || d.Inputs["server_secret"] != "sek" {
		t.Error("the server secret rides its own input, never the config")
	}
	// The image is digest-pinned before it reaches the template.
	if !strings.Contains(d.Inputs["collector_image"], "@sha256:") {
		t.Errorf("image should be pinned, got %s", d.Inputs["collector_image"])
	}
	// State is saved BEFORE the slow deploy.
	st, lerr := collector.LoadState()
	if lerr != nil || st == nil {
		t.Fatalf("state not saved: %v", lerr)
	}
	if st.AgentID != "agent-gcp" || !st.IsGCP() || st.Project != "acme-prod" || st.Region != "us-central1" || st.DeploymentName != "dbg-test" {
		t.Errorf("state = %+v", st)
	}
	// IAM auth needs the operator to register the service account as a user.
	if !strings.Contains(out, "gcloud sql users create dbg-test@acme-prod.iam.gserviceaccount.com") {
		t.Errorf("grant guidance should name the gcloud step, got:\n%s", out)
	}
}

func TestRunInstallGCP_DryRunMintsNothing(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubGCPOK(t)
	stubGcpDiscover(t, completeGcpTarget(), nil)
	deploys := stubGcpDeploy(t, nil)
	srv := installServer(t, "agent-gcp")
	defer srv.Close()

	c := gcpCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "dry-run", "true")
	mustSet(t, c, "deploy-service-account", "") // not needed to probe

	var err error
	out := capture(t, func() { err = runInstallGCP(c) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if !deploys.deploy.DryRun {
		t.Error("the deploy must be marked dry-run")
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("a dry run must not save state")
	}
	// The rendered inputs are shown with the config decoded to readable TOML.
	for _, want := range []string{"no identity minted", "collector_config =", `type = "cloud_sql"`, "runtime_service_account = dbg-test@acme-prod.iam.gserviceaccount.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestRunInstallGCP_FailedDeployRollsBackIdentityAndDeployment(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubGCPOK(t)
	stubGcpDiscover(t, completeGcpTarget(), nil)
	stubGcpDeploy(t, errors.New("terraform apply failed"))
	deleted := stubDeleteGcpDeployment(t, nil)
	srv := installServer(t, "agent-gcp")
	defer srv.Close()

	c := gcpCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallGCP(c) })
	if err == nil || !strings.Contains(err.Error(), "Rolled back") {
		t.Fatalf("a failed deploy must fail the install saying it rolled back, got %v", err)
	}
	if !*deleted {
		t.Error("the deployment must be deleted on rollback")
	}
	if !strings.Contains(out, "rolling back the provisioned identity and deployment") {
		t.Errorf("the rollback should be announced, got: %s", out)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("rollback must clear local state")
	}
}

// A deploy TIMEOUT is not a failure: rolling back would delete a deployment
// that is still converging.
func TestRunInstallGCP_TimeoutDoesNotRollBack(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubGCPOK(t)
	stubGcpDiscover(t, completeGcpTarget(), nil)
	stubGcpDeploy(t, collector.ErrDeployTimeout)
	deleted := stubDeleteGcpDeployment(t, nil)
	srv := installServer(t, "agent-gcp")
	defer srv.Close()

	c := gcpCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallGCP(c) })
	if err != nil {
		t.Fatalf("a timeout must not fail the install, got %v", err)
	}
	if *deleted {
		t.Error("a timeout must not delete the deployment")
	}
	if st, _ := collector.LoadState(); st == nil {
		t.Error("state must survive a timeout so status can find the deployment")
	}
	if !strings.Contains(out, "NOT rolled back") || !strings.Contains(out, "dbg collector status") {
		t.Errorf("the operator should be told to watch it, got: %s", out)
	}
}

func TestRunInstallGCP_PriorInstall(t *testing.T) {
	setup := func(t *testing.T) *cobra.Command {
		isolate(t)
		writeTokens(t)
		stubGCPOK(t)
		stubGcpDiscover(t, completeGcpTarget(), nil)
		stubGcpDeploy(t, nil)
		srv := installServer(t, "agent-new")
		t.Cleanup(srv.Close)
		c := gcpCmd(t)
		mustSet(t, c, "api-url", srv.URL)
		mustSet(t, c, "yes", "true")
		return c
	}

	t.Run("another target blocks", func(t *testing.T) {
		c := setup(t)
		if err := collector.SaveState(&collector.State{AgentID: "agent-aws", Target: "aws", StackName: "s"}); err != nil {
			t.Fatal(err)
		}
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err == nil || !strings.Contains(err.Error(), "already installed") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a live deployment is refused until uninstall", func(t *testing.T) {
		c := setup(t)
		if err := collector.SaveState(&collector.State{AgentID: "agent-old", Target: "gcp", Project: "acme-prod", Region: "us-central1", DeploymentName: "dbg-test"}); err != nil {
			t.Fatal(err)
		}
		stubGcpDeploymentStatus(t, "ACTIVE", nil)
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err == nil || !strings.Contains(err.Error(), "dbg collector uninstall") || !strings.Contains(err.Error(), "ACTIVE") {
			t.Fatalf("err = %v, want the uninstall hint with the state", err)
		}
	})

	t.Run("a vanished deployment installs fresh", func(t *testing.T) {
		c := setup(t)
		if err := collector.SaveState(&collector.State{AgentID: "agent-old", Target: "gcp", Project: "acme-prod", Region: "us-central1", DeploymentName: "dbg-test"}); err != nil {
			t.Fatal(err)
		}
		stubGcpDeploymentStatus(t, "", nil)
		var err error
		out := capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v\n%s", err, out)
		}
		if !strings.Contains(out, "no longer exists") || !strings.Contains(out, "agent-old") {
			t.Errorf("the stale record should be explained, got: %s", out)
		}
		if st, _ := collector.LoadState(); st == nil || st.AgentID != "agent-new" {
			t.Errorf("state should record the fresh install, got %+v", st)
		}
	})
}

// Flag mistakes are caught before a single cloud call is made.
func TestRunInstallGCP_MissingDeployServiceAccountFailsFirst(t *testing.T) {
	isolate(t)
	writeTokens(t)
	avail := stubGcpAvailable(t, nil)
	c := gcpCmd(t)
	mustSet(t, c, "api-url", "https://x")
	mustSet(t, c, "deploy-service-account", "")
	err := runInstallGCP(c)
	if err == nil || !strings.Contains(err.Error(), "--deploy-service-account") {
		t.Fatalf("err = %v", err)
	}
	if *avail != 0 {
		t.Error("the flag check must run before any Google Cloud call")
	}
}

func TestRunInstallGCP_Auth(t *testing.T) {
	setup := func(t *testing.T, target collector.GcpTarget) (*cobra.Command, *gcpDeployCall) {
		isolate(t)
		writeTokens(t)
		stubGCPOK(t)
		stubGcpDiscover(t, target, nil)
		deploys := stubGcpDeploy(t, nil)
		srv := installServer(t, "agent-gcp")
		t.Cleanup(srv.Close)
		c := gcpCmd(t)
		mustSet(t, c, "api-url", srv.URL)
		mustSet(t, c, "yes", "true")
		return c, deploys
	}

	t.Run("IAM off without a password is refused with the flag to flip", func(t *testing.T) {
		target := completeGcpTarget()
		target.IamEnabled = false
		c, _ := setup(t, target)
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err == nil || !strings.Contains(err.Error(), "cloudsql.iam_authentication") || !strings.Contains(err.Error(), "--db-password") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a password selects password auth with the collector's default user", func(t *testing.T) {
		target := completeGcpTarget()
		target.IamEnabled = false
		c, deploys := setup(t, target)
		mustSet(t, c, "db-password", "s3cret")
		var err error
		out := capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v\n%s", err, out)
		}
		cfg := decodedConfig(t, deploys.deploy)
		for _, want := range []string{`method = "password"`, `user = "` + collector.DefaultDBUser + `"`, `password = "${` + collector.CloudDBPasswordEnv + `}"`} {
			if !strings.Contains(cfg, want) {
				t.Errorf("config missing %s:\n%s", want, cfg)
			}
		}
		if deploys.deploy.Inputs["db_password"] != "s3cret" || strings.Contains(cfg, "s3cret") {
			t.Error("the password rides its own input, never the config")
		}
		if strings.Contains(out, "Grant the collector") {
			t.Error("password auth needs no IAM grant guidance")
		}
	})

	t.Run("--db-user names the password-auth user, on MySQL too", func(t *testing.T) {
		target := completeGcpTarget()
		target.Engine, target.Port, target.IamEnabled = "mysql", 3306, false
		c, deploys := setup(t, target)
		mustSet(t, c, "db-password", "s3cret")
		mustSet(t, c, "db-user", "dbg_ro")
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v", err)
		}
		cfg := decodedConfig(t, deploys.deploy)
		if !strings.Contains(cfg, `user = "dbg_ro"`) || strings.Contains(cfg, `"postgres"`) {
			t.Errorf("MySQL password auth must use the given user, never a postgres default:\n%s", cfg)
		}
	})

	t.Run("MySQL under IAM derives the short username", func(t *testing.T) {
		target := completeGcpTarget()
		target.Engine, target.Port = "mysql", 3306
		c, deploys := setup(t, target)
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v", err)
		}
		if cfg := decodedConfig(t, deploys.deploy); !strings.Contains(cfg, `user = "dbg-test"`) {
			t.Errorf("MySQL IAM user is the local part of the service account:\n%s", cfg)
		}
	})
}

// The query-analysis flags apply to gcp exactly as to aws.
func TestRunInstallGCP_CommandsFlags(t *testing.T) {
	run := func(t *testing.T, set func(*cobra.Command)) string {
		isolate(t)
		writeTokens(t)
		stubGCPOK(t)
		stubGcpDiscover(t, completeGcpTarget(), nil)
		deploys := stubGcpDeploy(t, nil)
		srv := installServer(t, "agent-gcp")
		t.Cleanup(srv.Close)
		c := gcpCmd(t)
		mustSet(t, c, "api-url", srv.URL)
		mustSet(t, c, "yes", "true")
		set(c)
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v", err)
		}
		return decodedConfig(t, deploys.deploy)
	}

	t.Run("default allows every command the engine supports", func(t *testing.T) {
		cfg := run(t, func(*cobra.Command) {})
		if !strings.Contains(cfg, `enabled = true`) || !strings.Contains(cfg, `commands = ["execute_query", "explain"]`) {
			t.Errorf("config:\n%s", cfg)
		}
	})
	t.Run("--commands narrows the set", func(t *testing.T) {
		cfg := run(t, func(c *cobra.Command) { mustSet(t, c, "commands", "explain") })
		if !strings.Contains(cfg, `commands = ["explain"]`) {
			t.Errorf("config:\n%s", cfg)
		}
	})
	t.Run(`--commands="" turns analysis off`, func(t *testing.T) {
		cfg := run(t, func(c *cobra.Command) { mustSet(t, c, "commands", "") })
		if !strings.Contains(cfg, `enabled = false`) || strings.Contains(cfg, "execute_query") {
			t.Errorf("config:\n%s", cfg)
		}
	})
	t.Run("--enable-commands=false turns analysis off", func(t *testing.T) {
		cfg := run(t, func(c *cobra.Command) { mustSet(t, c, "enable-commands", "false") })
		if !strings.Contains(cfg, `enabled = false`) {
			t.Errorf("config:\n%s", cfg)
		}
	})
}

func TestRunInstallGCP_NetworkComesFromTheDatabaseOrTheFlag(t *testing.T) {
	setup := func(t *testing.T, target collector.GcpTarget) (*cobra.Command, *gcpDeployCall) {
		isolate(t)
		writeTokens(t)
		stubGCPOK(t)
		stubGcpDiscover(t, target, nil)
		deploys := stubGcpDeploy(t, nil)
		srv := installServer(t, "agent-gcp")
		t.Cleanup(srv.Close)
		c := gcpCmd(t)
		mustSet(t, c, "api-url", srv.URL)
		mustSet(t, c, "yes", "true")
		return c, deploys
	}
	t.Run("no private network and no flag is an error naming --network", func(t *testing.T) {
		target := completeGcpTarget()
		target.Network = ""
		c, _ := setup(t, target)
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err == nil || !strings.Contains(err.Error(), "--network") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("--network overrides the discovered one", func(t *testing.T) {
		c, deploys := setup(t, completeGcpTarget())
		mustSet(t, c, "network", "projects/acme-prod/global/networks/shared")
		var err error
		capture(t, func() { err = runInstallGCP(c) })
		if err != nil {
			t.Fatalf("runInstallGCP: %v", err)
		}
		if got := deploys.deploy.Inputs["network"]; got != "projects/acme-prod/global/networks/shared" {
			t.Errorf("network = %q", got)
		}
	})
}

// With no --db-instance-id and a real terminal, an ambiguous project becomes
// a picker rather than an error — the same UX as the aws target.
func TestResolveGcpTarget_AmbiguityBecomesAPicker(t *testing.T) {
	isolate(t)
	setStdin(t, "2\n")
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = orig })

	amb := &collector.AmbiguousTargetError{Choices: []collector.TargetChoice{
		{ID: "prod-pg", ProviderType: "cloud_sql"},
		{ID: "orders/orders-primary", ProviderType: "alloydb"},
	}}
	var picked collector.TargetChoice
	origDiscover := discoverGcpTarget
	discoverGcpTarget = func(id, provider string, into collector.GcpTarget) (collector.GcpTarget, error) {
		if id == "" {
			return into, amb
		}
		picked = collector.TargetChoice{ID: id, ProviderType: provider}
		out := completeGcpTarget()
		out.ProviderType = provider
		return out, nil
	}
	t.Cleanup(func() { discoverGcpTarget = origDiscover })

	var got collector.GcpTarget
	var err error
	out := capture(t, func() { got, err = resolveGcpTarget(gcpCmd(t), "acme-prod") })
	if err != nil {
		t.Fatalf("resolveGcpTarget: %v", err)
	}
	if picked.ID != "orders/orders-primary" || picked.ProviderType != "alloydb" || got.ProviderType != "alloydb" {
		t.Errorf("the second candidate should be discovered with its provider type, got %+v", picked)
	}
	if !strings.Contains(out, "AlloyDB") || !strings.Contains(out, "Cloud SQL") {
		t.Errorf("candidates should be labelled by kind, got: %s", out)
	}
}

func TestResolveGcpTarget_AmbiguousNonInteractiveErrors(t *testing.T) {
	isolate(t)
	amb := &collector.AmbiguousTargetError{Choices: []collector.TargetChoice{
		{ID: "a", ProviderType: "cloud_sql"}, {ID: "b", ProviderType: "cloud_sql"},
	}}
	stubGcpDiscover(t, collector.GcpTarget{}, amb)
	c := gcpCmd(t)
	mustSet(t, c, "yes", "true")
	_, err := resolveGcpTarget(c, "acme-prod")
	if err == nil || !strings.Contains(err.Error(), "--db-instance-id") {
		t.Fatalf("ambiguity must not be resolved by guessing, got %v", err)
	}
}

// --- day 2 -----------------------------------------------------------------

func TestGcpStatus(t *testing.T) {
	isolate(t)
	st := &collector.State{
		AgentID: "agent-gcp", TenantID: "ten", Target: "gcp", TargetName: "prod-pg",
		Image: "img:v1", Project: "acme-prod", Region: "us-central1", DeploymentName: "dbg-test",
	}
	t.Run("reports the deployment state", func(t *testing.T) {
		stubGcpDeploymentStatus(t, "ACTIVE", nil)
		out := capture(t, func() {
			if err := gcpStatus(gcpCmd(t), st); err != nil {
				t.Fatalf("gcpStatus: %v", err)
			}
		})
		for _, want := range []string{"agent-gcp", "Deployment: dbg-test", "ACTIVE", "gcp — prod-pg"} {
			if !strings.Contains(out, want) {
				t.Errorf("output should contain %q, got: %s", want, out)
			}
		}
	})
	t.Run("a missing deployment says so", func(t *testing.T) {
		stubGcpDeploymentStatus(t, "", nil)
		out := capture(t, func() { _ = gcpStatus(gcpCmd(t), st) })
		if !strings.Contains(out, "deployment not found") {
			t.Errorf("out = %s", out)
		}
	})
	t.Run("an unreadable status is not fatal", func(t *testing.T) {
		stubGcpDeploymentStatus(t, "", errors.New("PERMISSION_DENIED"))
		out := capture(t, func() {
			if err := gcpStatus(gcpCmd(t), st); err != nil {
				t.Fatalf("status must not hard-fail, got %v", err)
			}
		})
		if !strings.Contains(out, "status unknown") {
			t.Errorf("out = %s", out)
		}
	})
}

func TestCollectorLifecycle_GCPRoutesToTheInstanceGroup(t *testing.T) {
	saveGCP := func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{AgentID: "a", Target: "gcp", Project: "acme-prod", Region: "us-central1", DeploymentName: "dbg-test"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("start and stop resize", func(t *testing.T) {
		saveGCP(t)
		var sizes []int
		orig := scaleGcpMig
		scaleGcpMig = func(project, region, name string, size int) error {
			if project != "acme-prod" || region != "us-central1" || name != "dbg-test" {
				t.Errorf("addressed %s/%s/%s", project, region, name)
			}
			sizes = append(sizes, size)
			return nil
		}
		t.Cleanup(func() { scaleGcpMig = orig })
		capture(t, func() {
			if err := stopCmd.RunE(stopCmd, nil); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if err := startCmd.RunE(startCmd, nil); err != nil {
				t.Fatalf("start: %v", err)
			}
		})
		if len(sizes) != 2 || sizes[0] != 0 || sizes[1] != 1 {
			t.Errorf("sizes = %v, want [0 1]", sizes)
		}
	})
	t.Run("restart recreates", func(t *testing.T) {
		saveGCP(t)
		called := false
		orig := restartGcpMig
		restartGcpMig = func(string, string, string) error { called = true; return nil }
		t.Cleanup(func() { restartGcpMig = orig })
		capture(t, func() {
			if err := restartCmd.RunE(restartCmd, nil); err != nil {
				t.Fatalf("restart: %v", err)
			}
		})
		if !called {
			t.Error("restart must recreate the instance group's instances")
		}
	})
	t.Run("logs tail Cloud Logging", func(t *testing.T) {
		saveGCP(t)
		var gotProject, gotName string
		orig := tailGcpLogs
		tailGcpLogs = func(project, name string, follow bool) error { gotProject, gotName = project, name; return nil }
		t.Cleanup(func() { tailGcpLogs = orig })
		if err := logsCmd.RunE(logsCmd, nil); err != nil {
			t.Fatalf("logs: %v", err)
		}
		if gotProject != "acme-prod" || gotName != "dbg-test" {
			t.Errorf("logs addressed %s/%s", gotProject, gotName)
		}
	})
	t.Run("upgrade refuses with the workaround", func(t *testing.T) {
		saveGCP(t)
		c := gcpCmd(t)
		err := runCollectorUpgrade(c, nil)
		if err == nil || !strings.Contains(err.Error(), "dbg collector uninstall") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunUninstall_GCPDeletesTheDeployment(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := statusServer(t, 204, "")
	defer srv.Close()
	if err := collector.SaveState(&collector.State{AgentID: "a1", Target: "gcp", Project: "acme-prod", Region: "us-central1", DeploymentName: "dbg-test"}); err != nil {
		t.Fatal(err)
	}
	deleted := stubDeleteGcpDeployment(t, nil)
	c := uninstallTestCmd()
	mustSet(t, c, "yes", "true")
	mustSet(t, c, "api-url", srv.URL)
	out := capture(t, func() {
		if err := runUninstall(c, nil); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
	})
	if !*deleted {
		t.Error("the deployment must be deleted")
	}
	if !strings.Contains(out, "Deployment dbg-test deleted") || !strings.Contains(out, "Identity deprovisioned") {
		t.Errorf("want full teardown:\n%s", out)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("state should be removed after successful deprovision")
	}
}
