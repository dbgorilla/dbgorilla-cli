package cmd

import (
	"context"
	"errors"
	"reflect"
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
	ensured  int
	admitted int
	released []collector.CollectorNetwork
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
		return nil
	}
	t.Cleanup(func() { ensureCollectorNetwork, admitCollector, releaseCollectorNetwork = oe, oa, or })
	return rec
}

func installAWS(t *testing.T, target collector.AwsTarget, flags map[string]string) (*deployCall, error, string) {
	t.Helper()
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubDiscover(t, target, nil)
	deploys := stubDeploy(t, nil)
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
	return deploys, err, out
}

// The task no longer runs in the database's group, which usually admits far
// more than the collector should be reachable from.
func TestRunInstallAWS_TheCollectorGetsItsOwnSecurityGroup(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	deploys, err, out := installAWS(t, discoveredTarget(), nil)
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
	deploys, err, out := installAWS(t, discoveredTarget(), map[string]string{"security-group-id": "sg-mine"})
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
	deploys, err, out := installAWS(t, completeTarget(), nil)
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if net.ensured != 0 || deploys.params["SecurityGroupId"] != "sg-db" {
		t.Errorf("ensured=%d sg=%q", net.ensured, deploys.params["SecurityGroupId"])
	}
}

func TestRunInstallAWS_ASecurityGroupFailureDeploysNothing(t *testing.T) {
	stubCollectorNetwork(t, errors.New("UnauthorizedOperation"))
	deploys, err, _ := installAWS(t, discoveredTarget(), nil)
	if err == nil {
		t.Fatal("expected the security group failure to stop the install")
	}
	if deploys.count != 0 {
		t.Errorf("deployed %d time(s) after the security group failed", deploys.count)
	}
}

func TestRunInstallAWS_DryRunCreatesNoSecurityGroup(t *testing.T) {
	net := stubCollectorNetwork(t, nil)
	_, err, out := installAWS(t, discoveredTarget(), map[string]string{"dry-run": "true"})
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
	out := capture(t, func() { err = runUpdateAWS(c, st) })
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
