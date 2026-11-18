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
			ec2ErrorXML("InvalidPermission.Duplicate", "already exists")).
		on("DescribeSecurityGroupRules", describeRuleXML("sg-existing", "added by hand")))

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

func describeOurGroupXML(id, extra string) string {
	return `<DescribeSecurityGroupsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <securityGroupInfo><item><groupId>` + id + `</groupId>` + extra + `</item></securityGroupInfo>
</DescribeSecurityGroupsResponse>`
}

// A group an earlier install made and could not remove is claimed, so this
// install's uninstall removes it. One the CLI did not make is left alone.
func TestAnOrphanedGroupOfOursIsClaimed(t *testing.T) {
	for name, tc := range map[string]struct {
		extra string
		ours  bool
	}{
		"our description": {`<groupDescription>` + collectorSGDescription + `</groupDescription>`, true},
		"our tag":         {`<tagSet><item><key>ManagedBy</key><value>dbgorilla-cli</value></item></tagSet>`, true},
		"someone else's":  {`<groupDescription>prod db</groupDescription>`, false},
	} {
		t.Run(name, func(t *testing.T) {
			stubAWS(t, newAWSFake(t).
				on("DescribeSubnets", describeSubnetsVPC1XML).
				on("DescribeSecurityGroups", describeOurGroupXML("sg-old", tc.extra)).
				on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
				on("AuthorizeSecurityGroupIngress", authorizeIngressXML))
			net, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
				[]string{"subnet-a"}, rdsTargets()[:1])
			if err != nil {
				t.Fatalf("EnsureCollectorNetwork: %v", err)
			}
			if net.SecurityGroupID != "sg-old" || net.SecurityGroupCreated != tc.ours {
				t.Fatalf("got %+v, want claimed=%v", net, tc.ours)
			}
		})
	}
}

// After the stack is gone, the task's interface can hold the group a little
// longer; the release retries rather than giving up at once.
func TestReleaseRetriesAGroupStillInUse(t *testing.T) {
	oa, od := sgReleaseAttempts, sgReleaseDelay
	sgReleaseAttempts, sgReleaseDelay = 3, 0
	t.Cleanup(func() { sgReleaseAttempts, sgReleaseDelay = oa, od })
	fake := newAWSFake(t).
		fail("DeleteSecurityGroup", http.StatusBadRequest, ec2ErrorXML("DependencyViolation", "in use"))
	stubAWS(t, fake)
	err := ReleaseCollectorNetwork(context.Background(), "us-east-1",
		CollectorNetwork{SecurityGroupID: "sg-own", SecurityGroupCreated: true})
	if err == nil {
		t.Fatal("expected the in-use group to be reported")
	}
	if n := fake.called("DeleteSecurityGroup"); n != 3 {
		t.Errorf("tried %d times, want 3", n)
	}
}

func describeRuleXML(groupRef, description string) string {
	return `<DescribeSecurityGroupRulesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <securityGroupRuleSet><item>
    <securityGroupRuleId>sgr-left</securityGroupRuleId><groupId>sg-db</groupId>
    <isEgress>false</isEgress><ipProtocol>tcp</ipProtocol><fromPort>5432</fromPort><toPort>5432</toPort>
    <referencedGroupInfo><groupId>` + groupRef + `</groupId></referencedGroupInfo>
    <description>` + description + `</description>
  </item></securityGroupRuleSet>
</DescribeSecurityGroupRulesResponse>`
}

// A rule left by an earlier run that failed before recording it must be
// claimed, or uninstall can never delete the collector's group: EC2 refuses
// while a rule still names it.
func TestALeftoverRuleOfOursIsClaimed(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", noSecurityGroupsXML).
		on("CreateSecurityGroup", createSGXML).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("RevokeSecurityGroupEgress", revokeEgressXML).
		fail("AuthorizeSecurityGroupIngress", http.StatusBadRequest,
			ec2ErrorXML("InvalidPermission.Duplicate", "already exists")).
		on("DescribeSecurityGroupRules", describeRuleXML("sg-new", ingressRuleMarker)))

	net, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, []AwsTarget{{Name: "pg", Port: 5432, DBSecurityGroups: []string{"sg-db"}}})
	if err != nil {
		t.Fatalf("EnsureCollectorNetwork: %v", err)
	}
	if !reflect.DeepEqual(net.Rules, []DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-left"}}) {
		t.Fatalf("leftover rule not claimed: %v", net.Rules)
	}
}

func TestAnExistingRuleWithoutOurMarkerIsNotClaimed(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", noSecurityGroupsXML).
		on("CreateSecurityGroup", createSGXML).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("RevokeSecurityGroupEgress", revokeEgressXML).
		fail("AuthorizeSecurityGroupIngress", http.StatusBadRequest,
			ec2ErrorXML("InvalidPermission.Duplicate", "already exists")).
		on("DescribeSecurityGroupRules", describeRuleXML("sg-new", "added by the platform team")))

	net, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, []AwsTarget{{Name: "pg", Port: 5432, DBSecurityGroups: []string{"sg-db"}}})
	if err != nil {
		t.Fatalf("EnsureCollectorNetwork: %v", err)
	}
	if len(net.Rules) != 0 {
		t.Fatalf("someone else's rule was recorded as ours: %v", net.Rules)
	}
}

// When undoing a half-made network also fails, the error must say what is
// left and how to remove it, rather than drop it.
func TestAFailedUndoNamesWhatIsLeft(t *testing.T) {
	fake := newAWSFake(t).
		on("DescribeSubnets", describeSubnetsVPC1XML).
		on("DescribeSecurityGroups", noSecurityGroupsXML).
		on("CreateSecurityGroup", createSGXML).
		on("AuthorizeSecurityGroupEgress", authorizeEgressXML).
		on("RevokeSecurityGroupEgress", revokeEgressXML).
		fail("AuthorizeSecurityGroupIngress", http.StatusForbidden,
			ec2ErrorXML("UnauthorizedOperation", "not authorized")).
		fail("DeleteSecurityGroup", http.StatusForbidden,
			ec2ErrorXML("UnauthorizedOperation", "not authorized"))
	stubAWS(t, fake)

	_, err := EnsureCollectorNetwork(context.Background(), "us-east-1", "dbg-collector",
		[]string{"subnet-a"}, rdsTargets()[:1])
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aws ec2 delete-security-group --group-id sg-new --region us-east-1") {
		t.Errorf("error does not say how to remove the group:\n%v", err)
	}
}

func TestManualNetworkCleanupRevokesRulesBeforeTheGroup(t *testing.T) {
	got := ManualNetworkCleanup("us-east-1", CollectorNetwork{
		SecurityGroupID: "sg-new", SecurityGroupCreated: true,
		Rules: []DBIngressRule{{GroupID: "sg-db", RuleID: "sgr-1"}},
	})
	revoke := strings.Index(got, "revoke-security-group-ingress --group-id sg-db --security-group-rule-ids sgr-1")
	del := strings.Index(got, "delete-security-group --group-id sg-new")
	if revoke < 0 || del < 0 || revoke > del {
		t.Fatalf("want the rule revoked before the group is deleted:\n%s", got)
	}
	if strings.Contains(ManualNetworkCleanup("us-east-1", CollectorNetwork{SecurityGroupID: "sg-x"}), "delete-security-group") {
		t.Error("a group this install did not create must not be deleted")
	}
}
