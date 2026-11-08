package collector

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	describeSubnetsVPC1XML = `<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <subnetSet><item><subnetId>subnet-a</subnetId><vpcId>vpc-1</vpcId></item></subnetSet>
</DescribeSubnetsResponse>`
	authorizeIngressXML = `<AuthorizeSecurityGroupIngressResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <return>true</return>
  <securityGroupRuleSet><item><securityGroupRuleId>sgr-1</securityGroupRuleId></item></securityGroupRuleSet>
</AuthorizeSecurityGroupIngressResponse>`
	revokeIngressXML = `<RevokeSecurityGroupIngressResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <return>true</return>
</RevokeSecurityGroupIngressResponse>`
)

func rdsTargets() []AwsTarget {
	return []AwsTarget{
		{Name: "pg", Port: 5432, DBSecurityGroups: []string{"sg-db"}},
		{Name: "my", Port: 3306, DBSecurityGroups: []string{"sg-db", "sg-other"}},
		{Name: "pg2", Port: 5432, DBSecurityGroups: []string{"sg-db"}},
	}
}

func TestDBIngressNeedsListsEachGroupAndPortOnce(t *testing.T) {
	got := DBIngressNeeds(rdsTargets())
	want := []DBIngressNeed{{"sg-db", 3306}, {"sg-db", 5432}, {"sg-other", 3306}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCollectorEgressPortsAreTheGatewayAndTheDatabasePorts(t *testing.T) {
	got := CollectorEgressPortsFor(rdsTargets())
	if !reflect.DeepEqual(got, []int32{443, 5432, 3306}) {
		t.Fatalf("got %v", got)
	}
}

func TestEnsureCollectorNetworkMakesANoIngressGroupAndAdmitsIt(t *testing.T) {
	fake := newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", noSecurityGroupsXML).
		on("CreateSecurityGroup", createSGXML).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("RevokeSecurityGroupEgress", revokeEgressXML).
		on("AuthorizeSecurityGroupIngress", authorizeIngressXML)
	stubAWS(t, fake)

	net, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, []AwsTarget{{Name: "pg", Port: 5432, DBSecurityGroups: []string{"sg-db"}}})
	if err != nil {
		t.Fatalf("EnsureCollectorNetwork: %v", err)
	}
	if net.SecurityGroupID != "sg-new" || !net.SecurityGroupCreated {
		t.Fatalf("group = %+v", net)
	}
	if !reflect.DeepEqual(net.Rules, []DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}}) {
		t.Fatalf("rules = %v", net.Rules)
	}
	body := fake.sentBody()
	// The ingress lands on the DATABASE's group, naming the collector's group.
	if !strings.Contains(body, "GroupId=sg-db") || !strings.Contains(body, "sg-new") {
		t.Errorf("ingress was not added to sg-db for sg-new:\n%s", body)
	}
	if fake.called("AuthorizeSecurityGroupIngress") != 1 {
		t.Errorf("want exactly one ingress rule, on the database's group")
	}
}

func TestAnExistingIngressRuleIsNotRecordedAsOurs(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", oneSecurityGroupXML("sg-existing")).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		fail("AuthorizeSecurityGroupIngress", http.StatusBadRequest,
			ec2ErrorXML("InvalidPermission.Duplicate", "already exists")))

	net, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, []AwsTarget{{Name: "pg", Port: 5432, DBSecurityGroups: []string{"sg-db"}}})
	if err != nil {
		t.Fatalf("EnsureCollectorNetwork: %v", err)
	}
	if net.SecurityGroupCreated || len(net.Rules) != 0 {
		t.Fatalf("a group and rule that already existed must not be marked ours: %+v", net)
	}
}

func TestAFailedIngressLeavesNothingBehind(t *testing.T) {
	fake := newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", noSecurityGroupsXML).
		on("CreateSecurityGroup", createSGXML).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("RevokeSecurityGroupEgress", revokeEgressXML).
		fail("AuthorizeSecurityGroupIngress", http.StatusForbidden,
			ec2ErrorXML("UnauthorizedOperation", "not authorized")).
		on("DeleteSecurityGroup", deleteSGXML)
	stubAWS(t, fake)

	if _, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, rdsTargets()[:1]); err == nil {
		t.Fatal("expected the ingress failure to surface")
	}
	if !called(fake, "DeleteSecurityGroup") {
		t.Error("the group made for this install was left behind")
	}
}

func TestReleaseRevokesTheDatabaseRulesBeforeDeletingTheGroup(t *testing.T) {
	fake := newAWSFake(t).
		on("RevokeSecurityGroupIngress", revokeIngressXML).
		on("DeleteSecurityGroup", deleteSGXML)
	stubAWS(t, fake)

	err := ReleaseCollectorNetwork(context.Background(), "us-east-1", CollectorNetwork{
		SecurityGroupID: "sg-new", SecurityGroupCreated: true,
		Rules: []DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}},
	})
	if err != nil {
		t.Fatalf("ReleaseCollectorNetwork: %v", err)
	}
	if want := []string{"RevokeSecurityGroupIngress", "DeleteSecurityGroup"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
}

func TestReleaseLeavesAGroupItDidNotMake(t *testing.T) {
	fake := newAWSFake(t).on("RevokeSecurityGroupIngress", revokeIngressXML)
	stubAWS(t, fake)
	if err := ReleaseCollectorNetwork(context.Background(), "us-east-1", CollectorNetwork{
		SecurityGroupID: "sg-existing", Rules: []DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if called(fake, "DeleteSecurityGroup") {
		t.Error("deleted a group this install did not create")
	}
}

func TestAdmitCollectorOpensANewDatabasePort(t *testing.T) {
	fake := newAWSFake(t).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("AuthorizeSecurityGroupIngress", authorizeIngressXML)
	stubAWS(t, fake)
	rules, err := AdmitCollector(context.Background(), "us-east-1", "sg-new",
		[]AwsTarget{{Name: "my", Port: 3306, DBSecurityGroups: []string{"sg-other"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rules, []DBIngressRule{{GroupID: "sg-other", RuleID: "sgr-1"}}) {
		t.Fatalf("rules = %v", rules)
	}
	if !strings.Contains(fake.sentBody(), "3306") {
		t.Error("egress to the new database port was not opened")
	}
}
