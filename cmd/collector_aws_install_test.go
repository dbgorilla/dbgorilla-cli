package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// The `--target aws` install is the most destructive path in the CLI: it mints
// an identity, creates a CloudFormation stack, and on failure rolls both back.
// These drive it end to end with every AWS call faked.

func TestRunInstallAWS_HappyPath(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if deploys.count != 1 {
		t.Fatalf("want exactly one deploy, got %d", deploys.count)
	}
	if deploys.stack != "dbg-collector" {
		t.Errorf("stack = %q", deploys.stack)
	}
	if deploys.dryRun {
		t.Error("a real install must not deploy in dry-run mode")
	}

	// State is saved BEFORE the slow deploy, so an interrupted install leaves a
	// collector that status/uninstall can find rather than an orphan.
	st, lerr := collector.LoadState()
	if lerr != nil || st == nil {
		t.Fatalf("state not saved: %v", lerr)
	}
	if st.AgentID != "agent-aws" || st.Target != "aws" || st.StackName != "dbg-collector" {
		t.Errorf("state = %+v", st)
	}
	if st.Region != "us-east-1" {
		t.Errorf("region should be captured at install time, got %q", st.Region)
	}
}

// A dry run must mint nothing and create nothing.
func TestRunInstallAWS_DryRunMintsNothing(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")
	mustSet(t, c, "dry-run", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if !deploys.dryRun {
		t.Error("the deploy must be marked dry-run")
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("a dry run must not save state")
	}
	if !strings.Contains(out, "no identity minted") {
		t.Errorf("output should say nothing was minted, got: %s", out)
	}
	// The placeholder identity keeps the template shape valid without
	// provisioning anything.
	if deploys.params["ServerSecret"] != "" && !strings.Contains(out, "DRY-RUN") {
		t.Error("dry run should use a placeholder identity")
	}
}

// A failed deploy rolls back BOTH the identity and the stack; leaving either
// behind means a customer pays for an orphan they cannot see.
func TestRunInstallAWS_FailedDeployRollsBackIdentityAndStack(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	stubDeploy(t, errors.New("CREATE_FAILED: insufficient capacity"))
	stackDeleted := stubDeleteStack(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err == nil {
		t.Fatal("a failed deploy must fail the install")
	}
	if !*stackDeleted {
		t.Error("the stack must be deleted on rollback")
	}
	if !strings.Contains(out, "rolling back") {
		t.Errorf("the rollback should be announced, got: %s", out)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("rollback must clear local state")
	}
	if !strings.Contains(err.Error(), "Rolled back") {
		t.Errorf("the error should say it rolled back, got: %v", err)
	}
}

// A deploy TIMEOUT is not a failure. Rolling back here would delete a stack
// that is still converging — the exact opposite of what the operator wants.
func TestRunInstallAWS_TimeoutDoesNotRollBack(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	stubDeploy(t, collector.ErrDeployTimeout)
	stackDeleted := stubDeleteStack(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("a timeout must not fail the command, got %v", err)
	}
	if *stackDeleted {
		t.Fatal("a still-converging stack must NOT be deleted")
	}
	if st, _ := collector.LoadState(); st == nil {
		t.Error("state must survive a timeout so status/uninstall still work")
	}
	if !strings.Contains(out, "NOT rolled back") {
		t.Errorf("the operator must be told it was left alone, got: %s", out)
	}
	if !strings.Contains(out, "dbg collector status") {
		t.Errorf("output should say how to watch it, got: %s", out)
	}
	// The removal hint must be a command uninstall accepts.
	if !strings.Contains(out, "dbg collector uninstall\n") || strings.Contains(out, "--stack-name") {
		t.Errorf("the removal hint should be plain `dbg collector uninstall`, got: %s", out)
	}
	// The stack was kept, so the grant the collector needs is still printed.
	if !strings.Contains(out, "GRANT rds_iam") {
		t.Errorf("grant guidance should still print when the stack is kept, got: %s", out)
	}
}

// A stack of that name with no local record: updating it in place would hand
// it a new identity and orphan the old one.
func TestRunInstallAWS_ExistingStackWithoutStateIsRefused(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubStackStatus(t, "CREATE_COMPLETE", nil)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--stack-name") {
		t.Fatalf("err = %v, want the existing-stack refusal", err)
	}
	if deploys.count != 0 {
		t.Error("nothing may be deployed over an unrecorded stack")
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("a refused install must not record state")
	}
}

// A parameter-rendering failure after the identity was minted must
// deprovision it: no state exists yet, so nothing else could.
func TestRunInstallAWS_ParamsFailureDeprovisionsTheIdentity(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0_2/collectors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"agent_id":"agent-aws","secret":"sek","tenant_id":"ten","domain":"dep.example"}`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/v0_2/collectors/agent-aws", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.Method == http.MethodDelete
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Enough databases to overflow the CloudFormation parameter.
	var cfg strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&cfg, "[[database]]\ninstance-id = \"prod-db-%02d\"\n\n", i)
	}
	path := filepath.Join(t.TempDir(), "dbs.toml")
	if err := os.WriteFile(path, []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "config", path)
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want the size error", err)
	}
	if !deleted {
		t.Error("the minted identity must be deprovisioned")
	}
	if deploys.count != 0 {
		t.Error("nothing may be deployed")
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Error("no state may be left behind")
	}
}

// --- preconditions --------------------------------------------------------

func TestRunInstallAWS_StopsOnMissingCredentials(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubAwsAvailable(t, errors.New("AWS credentials aren't working (run 'aws sso login')"))
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "sso login") {
		t.Fatalf("err = %v, want the credential error surfaced verbatim", err)
	}
	if deploys.count != 0 {
		t.Error("nothing should be deployed without working credentials")
	}
}

func TestRunInstallAWS_StopsWhenIdentityLookupFails(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubAwsIdentity(t, "", errors.New("expired token"))
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil {
		t.Fatal("expected the identity error")
	}
}

// A local Docker collector cannot be reconciled into an AWS one.
func TestRunInstallAWS_RefusesOverALocalCollector(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	if err := collector.SaveState(&collector.State{AgentID: "agent-local", Target: "docker"}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "yes", "true")
	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("err = %v, want a pointer at uninstall", err)
	}
}

// An existing AWS collector is an update, not a conflict.
func TestRunInstallAWS_ExistingStackBecomesAnUpdate(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	stubStackStatus(t, "CREATE_COMPLETE", nil)
	update := stubUpdateComponents(t, nil)
	deploys := stubDeploy(t, nil)
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Region: "us-east-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if !update.called {
		t.Fatal("an existing AWS stack should be updated in place")
	}
	if deploys.count != 0 {
		t.Error("an in-place update must not create a new stack")
	}
	if update.stack != "dbg-collector" || update.region != "us-east-1" {
		t.Errorf("update targeted %q/%q", update.stack, update.region)
	}
}

// State can name a stack that was deleted from the console. Updating it would
// fail deep in the SDK, so the install starts fresh instead.
func TestRunInstallAWS_VanishedStackInstallsFresh(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	stubStackStatus(t, "", nil) // gone
	update := stubUpdateComponents(t, nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()
	if err := collector.SaveState(&collector.State{
		AgentID: "old-agent", Target: "aws", StackName: "dbg-collector", Region: "us-east-1",
	}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if update.called {
		t.Error("a stack that no longer exists cannot be updated")
	}
	if deploys.count != 1 {
		t.Errorf("want a fresh deploy, got %d", deploys.count)
	}
	// The old identity is still provisioned server-side; the user has to be
	// told — and pointed at the console, since the install overwrites the
	// local record `uninstall` would need.
	if !strings.Contains(out, "old-agent") || !strings.Contains(out, "console") {
		t.Errorf("output should name the orphaned collector and how to remove it, got: %s", out)
	}
}

func TestRunInstallAWS_StackStatusErrorStops(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubStackStatus(t, "", errors.New("AccessDenied"))
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector",
	}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "yes", "true")
	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil {
		t.Fatal("an unreadable stack status must stop the install")
	}
}

func TestRunInstallAWS_MissingNetworkingIsActionable(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	// A target with no discovered subnets or security group.
	bare := completeTarget()
	bare.Subnets = nil
	bare.SecurityGroup = ""
	stubDiscover(t, bare, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "--subnets") {
		t.Fatalf("err = %v, want the flags that fix it named", err)
	}
}

// --- update in place ------------------------------------------------------

func TestRunUpdateAWS_PassesTheFullDesiredSet(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	update := stubUpdateComponents(t, nil)

	st := &collector.State{AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Region: "us-east-1"}
	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runUpdateAWS(c, st) })
	if err != nil {
		t.Fatalf("runUpdateAWS: %v\n%s", err, out)
	}
	if len(update.targets) != 1 {
		t.Fatalf("targets = %+v", update.targets)
	}
	// State's summary is refreshed so `status` reflects the new set.
	if st.TargetName == "" {
		t.Error("the state summary should be updated")
	}
}

// A rotated password has to reach the stack, or the collector is left with an
// unresolved reference and cannot authenticate.
func TestRunUpdateAWS_CarriesTheNewPassword(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	target := completeTarget()
	target.IAMAuthOn = false
	stubDiscover(t, target, nil)
	update := stubUpdateComponents(t, nil)

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "db-password", "rotated")
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() {
		err = runUpdateAWS(c, &collector.State{AgentID: "a", StackName: "s", Region: "us-east-1"})
	})
	if err != nil {
		t.Fatalf("runUpdateAWS: %v", err)
	}
	if update.password != "rotated" {
		t.Errorf("password = %q, want it carried through", update.password)
	}
}

func TestRunUpdateAWS_SurfacesTheUpdateError(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	stubUpdateComponents(t, errors.New("stack is UPDATE_IN_PROGRESS"))

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	capture(t, func() {
		err = runUpdateAWS(c, &collector.State{AgentID: "a", StackName: "s"})
	})
	if err == nil || !strings.Contains(err.Error(), "UPDATE_IN_PROGRESS") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunUpdateAWS_StopsWithoutCredentials(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	stubAwsAvailable(t, errors.New("no credentials"))

	c := awsCmd(t)
	mustSet(t, c, "yes", "true")
	var err error
	capture(t, func() { err = runUpdateAWS(c, &collector.State{}) })
	if err == nil {
		t.Fatal("expected the credential error")
	}
}

// --- grants ---------------------------------------------------------------

func TestApplyGrants_PrintsSQLByDefault(t *testing.T) {
	isolate(t)
	grants := stubRunGrant(t, nil)

	c := awsCmd(t)
	mustSet(t, c, "yes", "true") // non-interactive: never runs, always prints
	target := completeTarget()

	out := capture(t, func() { applyGrants(c, []collector.AwsTarget{target}) })
	if *grants != 0 {
		t.Error("a non-interactive run must not connect to the database")
	}
	if !strings.Contains(out, "GRANT") {
		t.Errorf("the SQL should be printed, got: %s", out)
	}
}

func TestApplyGrants_RunGrantExecutesTheStatements(t *testing.T) {
	isolate(t)
	grants := stubRunGrant(t, nil)

	c := awsCmd(t)
	mustSet(t, c, "run-grant", "true")
	mustSet(t, c, "grant-password", "admin-pw")
	mustSet(t, c, "yes", "true")

	out := capture(t, func() { applyGrants(c, []collector.AwsTarget{completeTarget()}) })
	if *grants != 1 {
		t.Fatalf("grant ran %d times, want 1", *grants)
	}
	if !strings.Contains(out, "Granted") {
		t.Errorf("success should be reported, got: %s", out)
	}
}

// A private database usually is not reachable from where the CLI runs. That
// must degrade to printing the SQL, never leave the user stuck.
func TestApplyGrants_UnreachableDatabaseFallsBackToPrinting(t *testing.T) {
	isolate(t)
	stubRunGrant(t, errors.New("dial tcp: i/o timeout"))

	c := awsCmd(t)
	mustSet(t, c, "run-grant", "true")
	mustSet(t, c, "grant-password", "admin-pw")
	mustSet(t, c, "yes", "true")

	out := capture(t, func() { applyGrants(c, []collector.AwsTarget{completeTarget()}) })
	if !strings.Contains(out, "GRANT") {
		t.Errorf("the SQL must still be printed when the run fails, got: %s", out)
	}
	if !strings.Contains(out, "private database") {
		t.Errorf("the likely cause should be named, got: %s", out)
	}
}

func TestApplyGrants_NoIAMTargetsIsSilent(t *testing.T) {
	isolate(t)
	c := awsCmd(t)
	mustSet(t, c, "yes", "true")

	// A password-auth database needs no IAM grant.
	target := completeTarget()
	target.AuthMethod = "password"
	out := capture(t, func() { applyGrants(c, []collector.AwsTarget{target}) })
	if strings.Contains(out, "GRANT") {
		t.Errorf("a password-auth database needs no grant, got: %s", out)
	}
}

// --- status / upgrade -----------------------------------------------------

func TestAwsStatus(t *testing.T) {
	isolate(t)
	st := &collector.State{
		AgentID: "agent-aws", TenantID: "ten", Target: "aws",
		TargetName: "prod", Image: "img:v1", StackName: "dbg-collector", Region: "us-east-1",
	}

	t.Run("reports the stack status", func(t *testing.T) {
		stubStackStatus(t, "CREATE_COMPLETE", nil)
		out := capture(t, func() {
			if err := awsStatus(awsCmd(t), st); err != nil {
				t.Fatalf("awsStatus: %v", err)
			}
		})
		for _, want := range []string{"agent-aws", "dbg-collector", "CREATE_COMPLETE"} {
			if !strings.Contains(out, want) {
				t.Errorf("output should contain %q, got: %s", want, out)
			}
		}
	})

	t.Run("a missing stack says so", func(t *testing.T) {
		stubStackStatus(t, "", nil)
		out := capture(t, func() { _ = awsStatus(awsCmd(t), st) })
		if !strings.Contains(out, "stack not found") {
			t.Errorf("out = %s", out)
		}
	})

	t.Run("an unreadable status is not fatal", func(t *testing.T) {
		stubStackStatus(t, "", errors.New("AccessDenied"))
		out := capture(t, func() {
			if err := awsStatus(awsCmd(t), st); err != nil {
				t.Fatalf("status must not hard-fail, got %v", err)
			}
		})
		if !strings.Contains(out, "status unknown") {
			t.Errorf("out = %s", out)
		}
	})
}

func TestRunCollectorUpgrade_AWSRollsTheImage(t *testing.T) {
	isolate(t)
	stubRemoteDigest(t, nil)
	got := stubUpgradeImage(t, nil)
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector",
		Region: "us-east-1", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "image", "ghcr.io/dbgorilla/collector:v2")

	var err error
	out := capture(t, func() { err = runCollectorUpgrade(c, nil) })
	if err != nil {
		t.Fatalf("runCollectorUpgrade: %v\n%s", err, out)
	}
	// CloudFormation must receive a digest, not a tag. A tag leaves the stack
	// parameter unchanged between releases, so CloudFormation finds nothing to
	// do and the upgrade silently does nothing -- while ECS re-pulls the tag on
	// any task restart, changing the version when nobody asked.
	want := "ghcr.io/dbgorilla/collector:v2@sha256:testdigest"
	if *got != want {
		t.Errorf("upgraded to %q, want the digest-pinned %q", *got, want)
	}
	// State has to move too, or `status` keeps reporting the old image.
	st, _ := collector.LoadState()
	if st == nil || st.Image != want {
		t.Errorf("state image = %+v, want %q", st, want)
	}
}

// If the registry cannot be reached, an AWS upgrade stops rather than sending
// a bare tag that CloudFormation would decline to act on.
func TestRunCollectorUpgrade_AWSStopsWhenTheDigestCannotBeResolved(t *testing.T) {
	isolate(t)
	stubRemoteDigest(t, errors.New("registry unreachable"))
	got := stubUpgradeImage(t, nil)
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector",
		Region: "us-east-1", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	capture(t, func() { err = runCollectorUpgrade(awsCmd(t), nil) })
	if err == nil {
		t.Fatal("expected the upgrade to stop")
	}
	if *got != "" {
		t.Errorf("nothing should have been sent to CloudFormation, got %q", *got)
	}
}

func TestRunCollectorUpgrade_AWSFailureStops(t *testing.T) {
	isolate(t)
	stubRemoteDigest(t, nil)
	stubUpgradeImage(t, errors.New("stack is UPDATE_IN_PROGRESS"))
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	capture(t, func() { err = runCollectorUpgrade(awsCmd(t), nil) })
	if err == nil {
		t.Fatal("expected the upgrade error")
	}
	// The recorded image must not move when the upgrade did not happen.
	st, _ := collector.LoadState()
	if st.Image != "img:v1" {
		t.Errorf("state image = %q, want the old one", st.Image)
	}
}

func TestRunCollectorUpgrade_NoCollectorInstalled(t *testing.T) {
	isolate(t)
	var err error
	capture(t, func() { err = runCollectorUpgrade(awsCmd(t), nil) })
	if err == nil {
		t.Fatal("upgrading with nothing installed must error")
	}
}

// An AWS install must not fail because a version lookup did. Deploying the tag
// is worse than deploying a digest, but far better than refusing to install --
// so it warns, says what the consequence is, and carries on.
func TestRunInstallAWS_UnresolvableDigestWarnsAndProceeds(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubRemoteDigest(t, errors.New("registry unreachable"))
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("a failed version lookup must not fail the install: %v\n%s", err, out)
	}
	if deploys.count != 1 {
		t.Fatalf("the stack should still deploy, got %d deploys", deploys.count)
	}
	// The operator has to learn what they traded away.
	if !strings.Contains(out, "restarts") {
		t.Errorf("the consequence should be stated, got: %s", out)
	}
}

// The happy path: the image reaching CloudFormation is digest-pinned, so the
// stack parameter changes when the collector does and not otherwise.
func TestRunInstallAWS_DeploysADigestPinnedImage(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	img := deploys.params["CollectorImage"]
	if !strings.Contains(img, "@sha256:") {
		t.Errorf("CollectorImage = %q, want a digest-pinned reference", img)
	}
}
