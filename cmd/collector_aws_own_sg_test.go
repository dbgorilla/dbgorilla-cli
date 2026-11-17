package cmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// discoveredTarget is a database whose security groups discovery found, the
// normal case for an RDS or Aurora install.
func discoveredTarget() collector.AwsTarget {
	t := completeTarget()
	t.DBSecurityGroups = []string{"sg-db"}
	return t
}

type networkRec struct {
	ensured    int
	admitted   int
	released   []collector.CollectorNetwork
	releaseErr error // what every release returns
}

func stubCollectorNetwork(t *testing.T, ensureErr error) *networkRec {
	t.Helper()
	rec := &networkRec{}
	oe, oa, or := ensureCollectorNetwork, admitCollector, releaseCollectorNetwork
	ensureCollectorNetwork = func(_ context.Context, _, _ string, _ []string, _ []collector.AwsTarget) (collector.CollectorNetwork, error) {
		rec.ensured++
		if ensureErr != nil {
			return collector.CollectorNetwork{}, ensureErr
		}
		return collector.CollectorNetwork{
			SecurityGroupID: "sg-own", SecurityGroupCreated: true,
			Rules: []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}},
		}, nil
	}
	admitCollector = func(_ context.Context, _, _ string, _ []collector.AwsTarget) ([]collector.DBIngressRule, error) {
		rec.admitted++
		return []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-2"}}, nil
	}
	releaseCollectorNetwork = func(_ context.Context, _ string, n collector.CollectorNetwork) error {
		rec.released = append(rec.released, n)
		return rec.releaseErr
	}
	t.Cleanup(func() { ensureCollectorNetwork, admitCollector, releaseCollectorNetwork = oe, oa, or })
	return rec
}

func installAWS(t *testing.T, target collector.AwsTarget, flags map[string]string) (*deployCall, string, error) {
	t.Helper()
	return installAWSDeploying(t, target, flags, nil)
}

// installAWSDeploying is installAWS with the deploy returning deployErr.
func installAWSDeploying(t *testing.T, target collector.AwsTarget, flags map[string]string, deployErr error) (*deployCall, string, error) {
	t.Helper()
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, target, nil)
	deploys := stubDeploy(t, deployErr)
	srv := installServer(t, "agent-aws")
	t.Cleanup(srv.Close)
	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")
	for k, v := range flags {
		mustSet(t, c, k, v)
	}
	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	return deploys, out, err
}

// The task no longer runs in the database's group, which usually admits far
// more than the collector should be reachable from.
func TestRunInstallAWS_TheCollectorGetsItsOwnSecurityGroup(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deploys, out, err := installAWS(t, discoveredTarget(), nil)
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if net.ensured != 1 {
		t.Fatalf("ensureCollectorNetwork called %d times", net.ensured)
	}
	if got := deploys.params["SecurityGroupId"]; got != "sg-own" {
		t.Errorf("task security group = %q, want the collector's own", got)
	}
	st, _ := collector.LoadState()
	if st == nil || st.CollectorSecurityGroupID != "sg-own" || !st.CollectorSecurityGroupCreated ||
		!reflect.DeepEqual(st.DBIngressRules, []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}}) {
		t.Errorf("state must record the group and rules for uninstall: %+v", st)
	}
}

func TestRunInstallAWS_AnExplicitSecurityGroupIsUsedAsIs(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deploys, out, err := installAWS(t, discoveredTarget(), map[string]string{"security-group-id": "sg-mine"})
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if net.ensured != 0 || deploys.params["SecurityGroupId"] != "sg-mine" {
		t.Errorf("ensured=%d sg=%q", net.ensured, deploys.params["SecurityGroupId"])
	}
}

// Without the database's groups there is nothing to admit a new group to, so
// the task stays where it can connect.
func TestRunInstallAWS_UnknownDatabaseGroupsKeepTheDatabaseGroup(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deploys, out, err := installAWS(t, completeTarget(), nil)
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if net.ensured != 0 || deploys.params["SecurityGroupId"] != "sg-db" {
		t.Errorf("ensured=%d sg=%q", net.ensured, deploys.params["SecurityGroupId"])
	}
}

func TestRunInstallAWS_ASecurityGroupFailureDeploysNothing(t *testing.T) {
	stubCollectorNetwork(t, errors.New("UnauthorizedOperation"))
	deploys, _, err := installAWS(t, discoveredTarget(), nil)
	if err == nil {
		t.Fatal("expected the security group failure to stop the install")
	}
	if deploys.count != 0 {
		t.Errorf("deployed %d time(s) after the security group failed", deploys.count)
	}
}

func TestRunInstallAWS_DryRunCreatesNoSecurityGroup(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	_, out, err := installAWS(t, discoveredTarget(), map[string]string{"dry-run": "true"})
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if net.ensured != 0 {
		t.Error("a dry run created a security group")
	}
}

// A database added on update must admit the collector's group, or the
// updated task cannot connect to it.
func TestRunUpdateAWS_AdmitsTheCollectorToANewDatabase(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	stubDiscover(t, discoveredTarget(), nil)
	stubUpdateComponents(t, nil)
	net := stubCollectorNetwork(t, nil)
	st := &collector.State{AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Region: "us-east-1",
		CollectorSecurityGroupID: "sg-own",
		DBIngressRules:           []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}}}
	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")
	var err error
	out := capture(t, func() { err = runUpdateAWS(c, st, false) })
	if err != nil {
		t.Fatalf("runUpdateAWS: %v\n%s", err, out)
	}
	if net.admitted != 1 || len(st.DBIngressRules) != 2 {
		t.Errorf("admitted=%d rules=%v", net.admitted, st.DBIngressRules)
	}
}

func TestReleaseOwnNetworkRevokesTheRecordedRules(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	rules := []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}}
	capture(t, func() {
		releaseOwnNetworkOrWarn(context.Background(), "us-east-1", collector.CollectorNetwork{Rules: rules})
		releaseOwnNetworkOrWarn(context.Background(), "us-east-1", collector.CollectorNetwork{})
	})
	if len(net.released) != 1 || !reflect.DeepEqual(net.released[0].Rules, rules) {
		t.Errorf("released %+v", net.released)
	}
}

// A rolled-back install removes the collector's own group as well as its
// database rules, once the stack is gone.
func TestRunInstallAWS_AFailedDeployRemovesTheGroupAndRules(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deleted := stubDeleteStack(t, nil)
	_, out, err := installAWSDeploying(t, discoveredTarget(), nil, errors.New("CREATE_FAILED"))
	if err == nil {
		t.Fatalf("expected the deploy failure to surface\n%s", out)
	}
	if !*deleted {
		t.Error("the stack was not deleted")
	}
	if len(net.released) != 1 || net.released[0].SecurityGroupID != "sg-own" || !net.released[0].SecurityGroupCreated ||
		len(net.released[0].Rules) != 1 {
		t.Fatalf("released %+v, want the group and its rule", net.released)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("nothing is left, so no record should be: %+v", st)
	}
}

// What the rollback cannot remove stays on record for uninstall to finish.
func TestRunInstallAWS_AFailedDeployKeepsARecordOfWhatItCouldNotRemove(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	net.releaseErr = errors.New("DependencyViolation")
	stubDeleteStack(t, nil)
	_, out, err := installAWSDeploying(t, discoveredTarget(), nil, errors.New("CREATE_FAILED"))
	if err == nil {
		t.Fatalf("expected the deploy failure to surface\n%s", out)
	}
	st, _ := collector.LoadState()
	if st == nil || !st.PendingNetworkCleanup || st.CollectorSecurityGroupID != "sg-own" ||
		!st.CollectorSecurityGroupCreated || len(st.DBIngressRules) != 1 {
		t.Fatalf("want a cleanup record for the group and rule, got %+v", st)
	}
	if !strings.Contains(out, "dbg collector uninstall") {
		t.Errorf("the user is not told how to finish:\n%s", out)
	}
}

// A busy stack means nothing was deployed, but the group and rules were made.
func TestRunInstallAWS_ABusyStackRemovesTheGroupAndRules(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deleted := stubDeleteStack(t, nil)
	_, out, err := installAWSDeploying(t, discoveredTarget(), nil, collector.ErrDeployBusy)
	if err == nil {
		t.Fatalf("expected the busy stack to surface\n%s", out)
	}
	if *deleted {
		t.Error("a busy stack is not ours to delete")
	}
	if len(net.released) != 1 || net.released[0].SecurityGroupID != "sg-own" {
		t.Fatalf("released %+v, want the group and its rule", net.released)
	}
}

// A record kept for cleanup blocks a fresh install, which would overwrite it.
func TestRunInstallAWS_AnUnfinishedCleanupBlocksTheInstall(t *testing.T) {
	stubCollectorNetwork(t, nil)
	isolate(t)
	if err := collector.SaveState(&collector.State{AgentID: "old", Target: "aws", StackName: "dbg-collector",
		Region: "us-east-1", CollectorSecurityGroupID: "sg-own", PendingNetworkCleanup: true}); err != nil {
		t.Fatal(err)
	}
	_, _, err := priorCloudInstall(false, (*collector.State).IsAWS,
		func(*collector.State) (string, error) { return "", nil }, "stack",
		func(st *collector.State) string { return st.StackName })
	if err == nil || !strings.Contains(err.Error(), "dbg collector uninstall") {
		t.Fatalf("err = %v, want a pointer to uninstall", err)
	}
}

func uninstallAWSState(t *testing.T) {
	t.Helper()
	if err := collector.SaveState(&collector.State{AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector",
		Region: "us-east-1", CollectorSecurityGroupID: "sg-own", CollectorSecurityGroupCreated: true,
		DBIngressRules: []collector.DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}}}); err != nil {
		t.Fatal(err)
	}
}

func runUninstallAWS(t *testing.T) (string, error) {
	t.Helper()
	srv := statusServer(t, 204, "")
	t.Cleanup(srv.Close)
	c := uninstallTestCmd()
	mustSet(t, c, "yes", "true")
	mustSet(t, c, "api-url", srv.URL)
	var err error
	out := capture(t, func() { err = runUninstall(c, nil) })
	return out, err
}

// Uninstall waits for the stack, then removes the rules and then the group.
func TestRunUninstall_AWSRemovesTheRulesThenTheGroup(t *testing.T) {
	isolate(t)
	writeTokens(t)
	net := stubCollectorNetwork(t, nil)
	released := stubReleaseCollectorSG(t, nil)
	deleted := stubDeleteStack(t, nil)
	uninstallAWSState(t)
	if out, err := runUninstallAWS(t); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !*deleted || len(net.released) != 1 || len(*released) != 1 || (*released)[0] != "sg-own" {
		t.Errorf("deleted=%v rules released=%+v group released=%v", *deleted, net.released, *released)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("state should be gone: %+v", st)
	}
}

// A group that cannot be removed yet stays on record, and a re-run of
// uninstall removes it without deprovisioning the identity again.
func TestRunUninstall_AWSGroupStillInUseIsRetriedByARerun(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubCollectorNetwork(t, nil)
	stubDeleteStack(t, nil)
	stubReleaseCollectorSG(t, errors.New("DependencyViolation"))
	uninstallAWSState(t)
	if out, err := runUninstallAWS(t); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	st, _ := collector.LoadState()
	if st == nil || !st.PendingNetworkCleanup || st.CollectorSecurityGroupID != "sg-own" || len(st.DBIngressRules) != 1 {
		t.Fatalf("want a cleanup record, got %+v", st)
	}

	released := stubReleaseCollectorSG(t, nil)
	// The identity is already gone: a deprovision call now would fail the run.
	srv := statusServer(t, 500, "")
	t.Cleanup(srv.Close)
	c := uninstallTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	var err error
	out := capture(t, func() { err = runUninstall(c, nil) })
	if err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if strings.Contains(out, "deprovision") {
		t.Errorf("the re-run tried to deprovision again:\n%s", out)
	}
	if len(*released) != 1 || (*released)[0] != "sg-own" {
		t.Errorf("group released %v", *released)
	}
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("state should be gone after the re-run: %+v", st)
	}
}

// The update dry run names the rules a real update would add, and adds none.
func TestRunUpdateAWS_DryRunShowsTheRulesItWouldAdd(t *testing.T) {
	isolate(t)
	stubAWSOK(t)
	stubDiscover(t, discoveredTarget(), nil)
	net := stubCollectorNetwork(t, nil)
	orig := previewUpdate
	previewUpdate = func(string, string, []collector.AwsTarget, bool) (string, error) { return "config", nil }
	t.Cleanup(func() { previewUpdate = orig })
	st := &collector.State{AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Region: "us-east-1",
		CollectorSecurityGroupID: "sg-own"}
	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")
	var err error
	out := capture(t, func() { err = runUpdateAWS(c, st, true) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if net.admitted != 0 {
		t.Error("a dry run admitted the collector")
	}
	if !strings.Contains(out, "sg-own") || !strings.Contains(out, "sg-db on port") {
		t.Errorf("the dry run does not show the rules it would add:\n%s", out)
	}
}
