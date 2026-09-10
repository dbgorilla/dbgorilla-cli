package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// Fakes for the Instaclustr seams, one per side effect, restored on cleanup.

func stubDiscoverInstaclustr(t *testing.T, target collector.InstaclustrTarget, err error) {
	t.Helper()
	orig := discoverInstaclustr
	discoverInstaclustr = func(_ context.Context, _ collector.InstaclustrCreds, _ string) (collector.InstaclustrTarget, error) {
		return target, err
	}
	t.Cleanup(func() { discoverInstaclustr = orig })
}

func stubEnsureFirewallRule(t *testing.T, rule collector.FirewallRule, created bool, err error) *[]string {
	t.Helper()
	var cidrs []string
	orig := ensureFirewallRule
	ensureFirewallRule = func(_ context.Context, _ collector.InstaclustrCreds, _ string, cidr string) (collector.FirewallRule, bool, error) {
		cidrs = append(cidrs, cidr)
		return rule, created, err
	}
	t.Cleanup(func() { ensureFirewallRule = orig })
	return &cidrs
}

func stubDeleteFirewallRule(t *testing.T, err error) *[]string {
	t.Helper()
	var deleted []string
	orig := deleteFirewallRule
	deleteFirewallRule = func(_ context.Context, _ collector.InstaclustrCreds, ruleID string) error {
		deleted = append(deleted, ruleID)
		return err
	}
	t.Cleanup(func() { deleteFirewallRule = orig })
	return &deleted
}

func stubCreateInstaclustrRole(t *testing.T, err error) *[]string {
	t.Helper()
	var runs []string
	orig := createInstaclustrRole
	createInstaclustrRole = func(_ context.Context, dsn, _, _ string) error {
		runs = append(runs, dsn)
		return err
	}
	t.Cleanup(func() { createInstaclustrRole = orig })
	return &runs
}

func stubPublicEgressIP(t *testing.T, ip string, err error) {
	t.Helper()
	orig := publicEgressIP
	publicEgressIP = func(_ context.Context) (string, error) { return ip, err }
	t.Cleanup(func() { publicEgressIP = orig })
}

func icTestTarget() collector.InstaclustrTarget {
	return collector.InstaclustrTarget{
		ClusterID: "c-1", Name: "orders", Status: "RUNNING",
		PostgresVersion: "18.4.0", CloudProvider: "AWS_VPC", Region: "US_EAST_1",
		DefaultUserPassword: "pw-1",
		Nodes: []collector.InstaclustrNode{
			{ID: "n1", PublicAddress: "203.0.113.10", PrivateAddress: "10.0.0.10"},
		},
	}
}

func icCmd(t *testing.T, apiURL string) *cobra.Command {
	t.Helper()
	cmd := baseCmd()
	cmd.Flags().String("provider", "", "")
	cmd.Flags().String("target", "", "")
	cmd.Flags().String("cluster-id", "", "")
	cmd.Flags().String("instaclustr-user", "", "")
	cmd.Flags().String("instaclustr-api-key", "", "")
	cmd.Flags().String("instaclustr-readonly-key", "", "")
	cmd.Flags().Bool("use-private-addresses", false, "")
	cmd.Flags().String("allow-ip", "", "")
	// Type must match the REAL registration (a plain String, CSV-split) — a
	// StringSlice here once masked a real GetStringSlice-on-String bug.
	cmd.Flags().String("db-name", "", "")
	cmd.Flags().String("ssl-mode", "verify-full", "")
	cmd.Flags().String("ca-cert", "", "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().String("image", "", "")
	cmd.Flags().String("auth-url", "", "")
	cmd.Flags().String("keycloak-url", "", "")
	cmd.Flags().String("otlp-url", "", "")
	cmd.Flags().String("opamp-url", "", "")
	if apiURL != "" {
		mustSet(t, cmd, "api-url", apiURL)
	}
	mustSet(t, cmd, "provider", "instaclustr")
	return cmd
}

func TestInstallInstaclustrRejectsNonDockerTargets(t *testing.T) {
	isolate(t)
	cmd := icCmd(t, "")
	mustSet(t, cmd, "target", "aws")
	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--target docker only") {
		t.Fatalf("expected the docker-only refusal, got %v", err)
	}
}

func TestInstallInstaclustrRequiresClusterIDNonInteractively(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), nil)
	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "instaclustr-user", "someone")
	mustSet(t, cmd, "instaclustr-api-key", "key123")
	mustSet(t, cmd, "instaclustr-readonly-key", "key456")
	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--cluster-id is required") {
		t.Fatalf("expected cluster-id requirement, got %v", err)
	}
}

func TestInstallInstaclustrRequiresCredsNonInteractively(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), nil)
	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "cluster-id", "c-1")
	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "INSTACLUSTR_USERNAME") {
		t.Fatalf("expected the username requirement naming the env var, got %v", err)
	}
}

func TestInstallInstaclustrHappyPath(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), nil)
	stubDiscoverInstaclustr(t, icTestTarget(), nil)
	stubPublicEgressIP(t, "192.0.2.9", nil)
	cidrs := stubEnsureFirewallRule(t, collector.FirewallRule{ID: "r-1", Network: "192.0.2.9/32"}, true, nil)
	roleRuns := stubCreateInstaclustrRole(t, nil)

	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "cluster-id", "c-1")
	mustSet(t, cmd, "instaclustr-user", "someone")
	mustSet(t, cmd, "instaclustr-api-key", "key123")
	mustSet(t, cmd, "instaclustr-readonly-key", "key456")

	if err := runInstall(cmd, nil); err != nil {
		t.Fatal(err)
	}

	if len(*cidrs) != 1 || (*cidrs)[0] != "192.0.2.9/32" {
		t.Fatalf("firewall not ensured for the egress IP: %v", *cidrs)
	}
	if len(*roleRuns) != 1 || !strings.Contains((*roleRuns)[0], "icpostgresql:pw-1@203.0.113.10:5432") {
		t.Fatalf("role creation used the wrong DSN: %v", *roleRuns)
	}

	st, err := collector.LoadState()
	if err != nil || st == nil {
		t.Fatalf("no state saved: %v", err)
	}
	if st.InstaclustrClusterID != "c-1" || st.FirewallRuleID != "r-1" || st.TargetName != "orders" {
		t.Fatalf("state missing instaclustr fields: %+v", st)
	}

	cfg, err := collector.LoadConfig(st.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Component[0].Provider
	if p.Type != "instaclustr" || p.ClusterID != "c-1" || p.CloudProvider != "AWS_VPC" ||
		p.APIUsername != "someone" || p.APIKey != "${"+collector.InstaclustrAPIKeyEnv+"}" {
		t.Fatalf("rendered provider block wrong: %+v", p)
	}
	if cfg.Component[0].Auth.User != collector.InstaclustrMonitorUser {
		t.Fatalf("auth user should be the monitoring role: %+v", cfg.Component[0].Auth)
	}
	// The env-file must carry the READ-ONLY key under the name the config
	// references — and never the writable setup key.
	env, err := os.ReadFile(st.EnvFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), collector.InstaclustrAPIKeyEnv+"=key456") {
		t.Fatalf("env-file missing the read-only key: %s", env)
	}
	if strings.Contains(string(env), "key123") {
		t.Fatalf("the writable setup key leaked into the env-file: %s", env)
	}
}

func TestInstallInstaclustrDryRunMutatesNothing(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), nil)
	stubDiscoverInstaclustr(t, icTestTarget(), nil)
	stubPublicEgressIP(t, "192.0.2.9", nil)
	cidrs := stubEnsureFirewallRule(t, collector.FirewallRule{}, false, errors.New("must not be called"))
	roleRuns := stubCreateInstaclustrRole(t, errors.New("must not be called"))

	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "cluster-id", "c-1")
	mustSet(t, cmd, "instaclustr-user", "someone")
	mustSet(t, cmd, "instaclustr-api-key", "key123")
	mustSet(t, cmd, "dry-run", "true")
	mustSet(t, cmd, "db-name", "orders,billing")

	out := capture(t, func() {
		if err := runInstall(cmd, nil); err != nil {
			t.Errorf("dry run failed: %v", err)
		}
	})
	if len(*cidrs) != 0 || len(*roleRuns) != 0 {
		t.Fatalf("dry run mutated: firewall=%v role=%v", *cidrs, *roleRuns)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Fatalf("dry run saved state: %+v", st)
	}
	if !strings.Contains(out, "192.0.2.9/32") || !strings.Contains(out, "type = \"instaclustr\"") {
		t.Fatalf("preview missing the firewall CIDR or rendered config:\n%s", out)
	}
	if !strings.Contains(out, `databases = ["orders", "billing"]`) {
		t.Fatalf("preview lost --db-name (CSV on a plain String flag):\n%s", out)
	}
	if strings.Contains(out, "key123") {
		t.Fatalf("the setup key leaked into the preview:\n%s", out)
	}
}

func TestInstallInstaclustrRollsBackTheRuleWhenTheContainerFails(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), errors.New("docker exploded"))
	stubDiscoverInstaclustr(t, icTestTarget(), nil)
	stubPublicEgressIP(t, "192.0.2.9", nil)
	stubEnsureFirewallRule(t, collector.FirewallRule{ID: "r-1", Network: "192.0.2.9/32"}, true, nil)
	stubCreateInstaclustrRole(t, nil)
	deleted := stubDeleteFirewallRule(t, nil)

	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "cluster-id", "c-1")
	mustSet(t, cmd, "instaclustr-user", "someone")
	mustSet(t, cmd, "instaclustr-api-key", "key123")
	mustSet(t, cmd, "instaclustr-readonly-key", "key456")

	if err := runInstall(cmd, nil); err == nil {
		t.Fatal("container failure must fail the install")
	}
	if len(*deleted) != 1 || (*deleted)[0] != "r-1" {
		t.Fatalf("the rule this run created was not rolled back: %v", *deleted)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Fatalf("state saved despite rollback: %+v", st)
	}
}

func TestInstallInstaclustrRejectsCACert(t *testing.T) {
	isolate(t)
	cmd := icCmd(t, "")
	mustSet(t, cmd, "ca-cert", "/tmp/some-ca.pem")
	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--ca-cert is not supported") {
		t.Fatalf("expected the ca-cert refusal, got %v", err)
	}
}

func TestInstallUnknownProviderIsRejected(t *testing.T) {
	isolate(t)
	cmd := icCmd(t, "")
	mustSet(t, cmd, "provider", "aws_rds")
	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown --provider") {
		t.Fatalf("an unknown provider must not fall through to the docker prompts, got %v", err)
	}
}

func TestInstallInstaclustrDiscoveryErrorSurfaces(t *testing.T) {
	isolate(t)
	writeTokens(t)
	srv := installServer(t, "a-1")
	defer srv.Close()
	setInstallStubs(t, nil, cleanReport(), nil)
	stubDiscoverInstaclustr(t, collector.InstaclustrTarget{}, errors.New("not found (HTTP 404)"))

	cmd := icCmd(t, srv.URL)
	mustSet(t, cmd, "cluster-id", "c-1")
	mustSet(t, cmd, "instaclustr-user", "someone")
	mustSet(t, cmd, "instaclustr-api-key", "key123")
	mustSet(t, cmd, "instaclustr-readonly-key", "key456")

	err := runInstall(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected the discovery error to surface, got %v", err)
	}
}

func TestRefreshFirewallRequiresAnInstaclustrInstall(t *testing.T) {
	isolate(t)
	if err := collector.SaveState(&collector.State{AgentID: "a-1"}); err != nil {
		t.Fatal(err)
	}
	err := runRefreshFirewall(refreshFirewallCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--provider instaclustr") {
		t.Fatalf("expected the not-an-instaclustr-install refusal, got %v", err)
	}
}

func TestRefreshFirewallRotatesTheRule(t *testing.T) {
	isolate(t)
	if err := collector.SaveState(&collector.State{
		AgentID:              "a-1",
		InstaclustrClusterID: "c-1",
		InstaclustrUsername:  "someone",
		FirewallRuleID:       "r-old",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(instaclustrProvisioningEnv, "key123")
	stubPublicEgressIP(t, "192.0.2.9", nil)
	cidrs := stubEnsureFirewallRule(t, collector.FirewallRule{ID: "r-new", Network: "192.0.2.9/32"}, true, nil)
	deleted := stubDeleteFirewallRule(t, nil)

	if err := runRefreshFirewall(refreshFirewallCmd, nil); err != nil {
		t.Fatal(err)
	}
	if len(*cidrs) != 1 || (*cidrs)[0] != "192.0.2.9/32" {
		t.Fatalf("wrong CIDR ensured: %v", *cidrs)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "r-old" {
		t.Fatalf("stale rule not rotated out: %v", *deleted)
	}
	st, err := collector.LoadState()
	if err != nil || st.FirewallRuleID != "r-new" {
		t.Fatalf("state not updated with the new rule: %+v err=%v", st, err)
	}
}

func TestRefreshFirewallNeverDeletesARuleItDoesNotOwn(t *testing.T) {
	isolate(t)
	if err := collector.SaveState(&collector.State{
		AgentID:              "a-1",
		InstaclustrClusterID: "c-1",
		InstaclustrUsername:  "someone",
		// No FirewallRuleID: the install found the rule pre-existing.
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(instaclustrProvisioningEnv, "key123")
	stubPublicEgressIP(t, "192.0.2.9", nil)
	stubEnsureFirewallRule(t, collector.FirewallRule{ID: "r-x", Network: "192.0.2.9/32"}, false, nil)
	deleted := stubDeleteFirewallRule(t, errors.New("must not be called"))

	if err := runRefreshFirewall(refreshFirewallCmd, nil); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 0 {
		t.Fatalf("deleted a rule the CLI does not own: %v", *deleted)
	}
}
