package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// helmTestCmd mirrors helmValuesCmd's flag set without registering the real
// command, matching installTestCmd's approach.
func helmTestCmd() *cobra.Command {
	c := &cobra.Command{}
	f := c.Flags()
	f.String("api-url", "", "")
	f.Bool("insecure", false, "")
	f.String("name", "", "")
	f.String("namespace", "", "")
	f.String("cluster", "", "")
	f.String("db-name", "", "")
	f.String("db-user", collector.DefaultDBUser, "")
	f.String("ssl-mode", "verify-full", "")
	f.String("k8s-mode", collector.K8sModeAuto, "")
	f.Int("metrics-port", collector.DefaultMetricsPort, "")
	f.Bool("metrics-tls", false, "")
	f.String("metrics-ca", "", "")
	f.String("release-name", collector.DefaultReleaseName, "")
	f.String("release-namespace", collector.DefaultReleaseNamespace, "")
	f.String("secret-name", "dbg-collector-secrets", "")
	f.Bool("enable-commands", false, "")
	f.Bool("dry-run", false, "")
	f.Bool("yes", false, "")
	f.String("auth-url", "", "")
	f.String("otlp-url", "", "")
	f.String("opamp-url", "", "")
	return c
}

func helmDryRunCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := helmTestCmd()
	mustSet(t, c, "dry-run", "true")
	mustSet(t, c, "namespace", "prod-db")
	mustSet(t, c, "cluster", "app-db")
	mustSet(t, c, "db-user", "dbg_readonly")
	return c
}

// A dry run must not need a login, an API URL, or the network -- it is the way
// an operator sees exactly what would be handed to their platform team before
// any identity exists.
func TestRunHelmValues_DryRunNeedsNothing(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)

	var err error
	out := capture(t, func() { err = runHelmValues(c, nil) })
	if err != nil {
		t.Fatalf("dry run should not error: %v", err)
	}
	for _, want := range []string{
		"DRY RUN",
		"type = \"cnpg\"",
		"namespace = \"prod-db\"",
		"cluster = \"app-db\"",
		"kubectl create secret generic dbg-collector-secrets",
		"helm install dbg-collector oci://",
		"--set-file config.inline=",
		"cnpg:prod-db/app-db",
		"app-db-rw.prod-db:5432",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n---\n%s", want, out)
		}
	}
}

// The secret is never rendered into anything pasteable.
func TestRunHelmValues_DryRunLeaksNoLiterals(t *testing.T) {
	isolate(t)
	out := capture(t, func() { _ = runHelmValues(helmDryRunCmd(t), nil) })
	if !strings.Contains(out, "${"+collector.SecretEnv+"}") {
		t.Error("expected the server secret to stay an ${ENV} reference")
	}
	if strings.Contains(out, "<minted-on-run>\"\n") && !strings.Contains(out, "DRY RUN") {
		t.Error("placeholder identity should only appear on the dry-run path")
	}
}

// A dry run writes nothing: no config file, no state.
func TestRunHelmValues_DryRunWritesNothing(t *testing.T) {
	isolate(t)
	_ = capture(t, func() { _ = runHelmValues(helmDryRunCmd(t), nil) })
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("dry run must not save state, got %+v", st)
	}
}

// --cluster is the flag most likely to be left off, because the issue this
// feature came from said it was optional. The error has to explain itself.
func TestRunHelmValues_MissingClusterExplains(t *testing.T) {
	isolate(t)
	c := helmTestCmd()
	mustSet(t, c, "dry-run", "true")
	mustSet(t, c, "namespace", "prod-db")

	err := runHelmValues(c, nil)
	if err == nil {
		t.Fatal("expected an error when --cluster is absent")
	}
	if !strings.Contains(err.Error(), "--cluster is required") {
		t.Errorf("error should name the flag, got %q", err)
	}
}

func TestRunHelmValues_RejectsBadK8sMode(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)
	mustSet(t, c, "k8s-mode", "occasionally")
	err := runHelmValues(c, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown --k8s-mode") {
		t.Fatalf("want an unknown-mode error, got %v", err)
	}
}

// metrics-server is a cluster component the collector needs and does not
// install. Managed clusters ship it, self-managed ones often do not, and when
// it is missing the API group does not exist at all -- so the permission grant
// for it applies cleanly and grants access to nothing. The handover names it
// while the operator is still at a terminal.
func TestPrintHelmHandover_NamesThePodMetricsPrerequisite(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)

	var err error
	out := capture(t, func() { err = runHelmValues(c, nil) })
	if err != nil {
		t.Fatalf("dry run should proceed: %v", err)
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "metrics-server") {
		t.Errorf("handover should name metrics-server as a prerequisite\n---\n%s", out)
	}
	// The check must exercise the path, in the namespace the collector will use,
	// not read the API's registration. The registration is an aggregated one:
	// it can report the API present while the server behind it is down, so
	// "installed" and "working" are separate facts and only the second is the
	// one the operator needs.
	if !strings.Contains(low, "kubectl top pods -n") {
		t.Errorf("handover should suggest the namespaced check that exercises the path\n---\n%s", out)
	}
	if strings.Contains(low, "apiservice") {
		t.Errorf("do not send the operator to the APIService registration: it reports installed, not working\n---\n%s", out)
	}
	// Named namespace, not a placeholder -- a command the reader has to edit
	// before running is a command they will get wrong or skip.
	if !strings.Contains(out, "kubectl top pods -n prod-db") {
		t.Errorf("the check command should name the target namespace\n---\n%s", out)
	}
	// The scoping half, and the reason this assertion exists: an earlier draft
	// of this text said CPU, memory AND disk were lost without metrics-server.
	// Disk comes from the volume records in the core API and is unaffected.
	// Saying otherwise sends someone installing a component that was never the
	// reason their disk panel is empty.
	if !strings.Contains(low, "disk use is not affected") {
		t.Errorf("the prerequisite must exclude disk explicitly, not sweep all three together\n---\n%s", out)
	}
}

// The prerequisite is about the pod-metrics API, which metrics-only mode never
// consults. Printing it there would tell an operator to go install something
// their chosen mode has no use for.
func TestPrintHelmHandover_SkipsPrerequisiteWhenMetricsOnly(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)
	mustSet(t, c, "k8s-mode", collector.K8sModeDisabled)
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runHelmValues(c, nil) })
	if err != nil {
		t.Fatalf("metrics-only dry run should proceed: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "metrics-server") {
		t.Errorf("metrics-only mode should not ask for a component it never uses\n---\n%s", out)
	}
}

// Choosing metrics-only silently drops backup and recoverability collection.
// Nothing downstream ever says so, so the command must -- and must name the
// capabilities lost, not the setting that was changed.
func TestRunHelmValues_MetricsOnlyWarnsAboutBackups(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)
	mustSet(t, c, "k8s-mode", collector.K8sModeDisabled)
	mustSet(t, c, "yes", "true") // non-interactive confirm

	var err error
	out := capture(t, func() { err = runHelmValues(c, nil) })
	if err != nil {
		t.Fatalf("metrics-only dry run should proceed: %v", err)
	}
	low := strings.ToLower(out)
	for _, want := range []string{"metrics-only", "backup", "wal", "cpu", "memory", "disk"} {
		if !strings.Contains(low, want) {
			t.Errorf("metrics-only warning should mention %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "no reinstall needed") {
		t.Error("the warning should say the mode is reversible with a permission grant")
	}
	// The mode's name is the trap: an operator reads "metrics-only" as "metrics
	// still work". CPU and memory come from the cluster's pod-metrics API, not
	// from the database's own endpoint, so the warning has to say which metrics
	// it means. Deleting that sentence as redundant is the regression.
	if !strings.Contains(low, "cluster api") {
		t.Errorf("the warning must say the resource figures come from the cluster API, not the database's own metrics endpoint\n---\n%s", out)
	}
}

// TLS keys only appear when the operator says the Cluster enables them; the
// CNPG default on 1.29/1.30 is plain HTTP.
func TestRunHelmValues_TLSOnlyWhenAsked(t *testing.T) {
	isolate(t)
	plain := capture(t, func() { _ = runHelmValues(helmDryRunCmd(t), nil) })
	if strings.Contains(plain, "server_name") {
		t.Errorf("no TLS keys expected by default:\n%s", plain)
	}

	c := helmDryRunCmd(t)
	mustSet(t, c, "metrics-tls", "true")
	mustSet(t, c, "metrics-ca", "/etc/ca/ca.crt")
	withTLS := capture(t, func() { _ = runHelmValues(c, nil) })
	if !strings.Contains(withTLS, `server_name = "app-db-rw"`) {
		t.Errorf("expected the role-service name to be verified:\n%s", withTLS)
	}
}

func TestRunHelmValues_TLSWithoutCARejected(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)
	mustSet(t, c, "metrics-tls", "true")
	err := runHelmValues(c, nil)
	if err == nil || !strings.Contains(err.Error(), "needs --metrics-ca") {
		t.Fatalf("want a missing-CA error, got %v", err)
	}
}

// --- the list/status contradiction ----------------------------------------

// A Helm-installed collector exists as far as `dbg collector list` is concerned,
// because that reads the backend. Before this, `status` read local state and
// reported none installed -- the two disagreeing reads as a broken CLI.
func TestHelmStatus_ReportsTheReleaseInsteadOfClaimingNothingIsInstalled(t *testing.T) {
	isolate(t)
	st := &collector.State{
		AgentID: "agent-h", TenantID: "tenant-h", Target: "helm",
		TargetName:  "cnpg:prod-db/app-db",
		ReleaseName: "dbg-collector", ReleaseNamespace: "dbg-collector",
	}
	c := helmTestCmd()
	out := capture(t, func() {
		if err := helmStatus(c, st); err != nil {
			t.Fatalf("helmStatus: %v", err)
		}
	})
	for _, want := range []string{"agent-h", "cnpg — cnpg:prod-db/app-db", "dbg-collector", "in-cluster", "kubectl"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "No collector installed") {
		t.Error("a provisioned Helm collector must not be reported as absent")
	}
}

// The lifecycle verbs drive a runtime this CLI started. For an in-cluster
// release they must say so and name the equivalent command, not fail with a
// Docker error about a collector that is running fine in Kubernetes.
func TestRequireRunnableState_RejectsHelmWithTheRealCommand(t *testing.T) {
	isolate(t)
	if err := collector.SaveState(&collector.State{
		AgentID: "a", Target: "helm",
		ReleaseName: "dbg-collector", ReleaseNamespace: "dbg-collector",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := requireRunnableState("logs")
	if err == nil {
		t.Fatal("expected logs to be refused for a Helm collector")
	}
	for _, want := range []string{"runs in your cluster", "kubectl", "helm upgrade", "status", "uninstall"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should point at the real command, missing %q:\n%s", want, err)
		}
	}
}

func TestRequireRunnableState_AllowsDockerAndAWS(t *testing.T) {
	isolate(t)
	for _, target := range []string{"", "docker", "aws"} {
		if err := collector.SaveState(&collector.State{AgentID: "a", Target: target}); err != nil {
			t.Fatal(err)
		}
		if _, err := requireRunnableState("logs"); err != nil {
			t.Errorf("target %q should be runnable, got %v", target, err)
		}
	}
}

// --- the minted path ------------------------------------------------------

// The full run: mint an identity, write the config, save state. State is the
// whole point -- it is what makes `status` agree with `list`.
func TestRunHelmValues_MintsAndRecordsState(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "agent-cnpg")
	defer srv.Close()

	c := helmTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "namespace", "prod-db")
	mustSet(t, c, "cluster", "app-db")
	mustSet(t, c, "db-user", "dbg_readonly")

	var err error
	out := capture(t, func() { err = runHelmValues(c, nil) })
	if err != nil {
		t.Fatalf("helm-values: %v", err)
	}
	for _, want := range []string{
		"Collector provisioned (agent agent-cnpg", "Wrote config",
		"only time the collector's secret is shown", "export " + collector.SecretEnv,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}

	st, err := collector.LoadState()
	if err != nil || st == nil {
		t.Fatalf("expected saved state, got %v (%v)", st, err)
	}
	if !st.IsHelm() {
		t.Errorf("state target = %q, want helm", st.Target)
	}
	if st.TargetName != "cnpg:prod-db/app-db" {
		t.Errorf("state should record the stable key, got %q", st.TargetName)
	}
	if st.DBNamespace != "prod-db" || st.DBCluster != "app-db" {
		t.Errorf("state should record the monitored cluster, got %q/%q", st.DBNamespace, st.DBCluster)
	}
	if st.ContainerName != "" || st.StackName != "" {
		t.Errorf("a Helm collector has no container and no stack: %+v", st)
	}
}

// An existing collector of any kind blocks a second one, exactly as the docker
// and aws paths do -- one state file, one collector.
func TestRunHelmValues_RefusesWhenACollectorExists(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "agent-cnpg")
	defer srv.Close()
	if err := collector.SaveState(&collector.State{AgentID: "already-here"}); err != nil {
		t.Fatal(err)
	}
	c := helmTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "namespace", "prod-db")
	mustSet(t, c, "cluster", "app-db")

	err := runHelmValues(c, nil)
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("want an already-installed error, got %v", err)
	}
}

// The fixture above mirrors the real command's flags rather than being it, so
// the two can drift -- and did: `--yes` was read by runHelmValues and never
// registered, which every fixture-based test missed because the fixture had it.
// This asserts the real command carries every flag the code reads.
func TestHelmValuesCmd_RegistersEveryFlagTheCodeReads(t *testing.T) {
	for _, name := range []string{
		"name", "namespace", "cluster", "db-name", "db-user", "ssl-mode",
		"k8s-mode", "metrics-port", "metrics-tls", "metrics-ca",
		"release-name", "release-namespace", "secret-name",
		"enable-commands", "yes", "dry-run",
		"auth-url", "otlp-url", "opamp-url",
	} {
		if helmValuesCmd.Flags().Lookup(name) == nil {
			t.Errorf("helm-values does not register --%s, but the code reads it", name)
		}
	}
}

// And the fixture must not drift the other way either: a flag the fixture has
// and the real command lacks makes every test using it a false pass.
func TestHelmTestCmd_MatchesTheRealCommand(t *testing.T) {
	helmTestCmd().Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "insecure" || f.Name == "api-url" {
			return // supplied by the root command in production
		}
		if helmValuesCmd.Flags().Lookup(f.Name) == nil {
			t.Errorf("test fixture has --%s but the real command does not", f.Name)
		}
	})
}

// --- the failure paths on the minted run -----------------------------------

// Every early return in the minted path is a case where the user has typed a
// real command and something is wrong. Each must fail with an actionable
// message rather than proceeding to mint an identity nobody can use.
func TestRunHelmValues_MintedPathFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T) *cobra.Command
		want  string
	}{
		"no api url": {
			func(t *testing.T) *cobra.Command {
				isolate(t)
				writeTokens(t)
				c := helmTestCmd()
				mustSet(t, c, "namespace", "prod-db")
				mustSet(t, c, "cluster", "app-db")
				return c // api-url unset and no ambient default
			},
			// Deliberately not asserting the wording: with no API URL the shared
			// precondition helpers report an expired login rather than a missing
			// URL. That is pre-existing behaviour in a helper this change does not
			// touch. What matters here is that the run stops and mints nothing.
			"",
		},
		"not logged in": {
			func(t *testing.T) *cobra.Command {
				isolate(t) // no writeTokens
				srv := installServer(t, "agent-x")
				t.Cleanup(srv.Close)
				c := helmTestCmd()
				mustSet(t, c, "api-url", srv.URL)
				mustSet(t, c, "namespace", "prod-db")
				mustSet(t, c, "cluster", "app-db")
				return c
			},
			"login",
		},
		"backend unreachable": {
			func(t *testing.T) *cobra.Command {
				isolate(t)
				writeTokens(t)
				c := helmTestCmd()
				// A port nothing is listening on: CollectorSupported must error,
				// and the command must not mint against an unreachable backend.
				mustSet(t, c, "api-url", "http://127.0.0.1:1")
				mustSet(t, c, "namespace", "prod-db")
				mustSet(t, c, "cluster", "app-db")
				return c
			},
			"",
		},
	} {
		c := tc.setup(t)
		var err error
		_ = capture(t, func() { err = runHelmValues(c, nil) })
		if err == nil {
			t.Errorf("%s: expected an error, got nil", name)
			continue
		}
		if tc.want != "" && !strings.Contains(strings.ToLower(err.Error()), tc.want) {
			t.Errorf("%s: error %q should mention %q", name, err, tc.want)
		}
		// Nothing may be left behind by a failed run.
		if st, _ := collector.LoadState(); st != nil {
			t.Errorf("%s: failed run saved state: %+v", name, st)
		}
	}
}

// A deployment that does not offer the managed collector must say so, rather
// than minting an identity its backend will not accept.
func TestRunHelmValues_UnsupportedDeployment(t *testing.T) {
	isolate(t)
	writeTokens(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0_2/collectors", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // capability probe says "not here"
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := helmTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "namespace", "prod-db")
	mustSet(t, c, "cluster", "app-db")

	var err error
	_ = capture(t, func() { err = runHelmValues(c, nil) })
	if err == nil {
		t.Fatal("expected an error when the deployment has no managed collector")
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("unsupported deployment must not save state, got %+v", st)
	}
}

// Declining the metrics-only warning aborts before anything is minted. The
// warning exists to be actionable, which means "no" has to actually stop.
func TestRunHelmValues_MetricsOnlyDeclineAborts(t *testing.T) {
	isolate(t)
	c := helmDryRunCmd(t)
	mustSet(t, c, "k8s-mode", collector.K8sModeDisabled)
	// yes=false and no terminal -> confirm() declines.

	var err error
	_ = capture(t, func() { err = runHelmValues(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("declining metrics-only should abort, got %v", err)
	}
}
