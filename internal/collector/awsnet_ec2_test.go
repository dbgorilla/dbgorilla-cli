package collector

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Network verification answers "would the collector be allowed to reach this
// database" before a stack is created. Getting it wrong either blocks a working
// install or, worse, waves through one that will silently never connect.

func ec2Client(t *testing.T) *ec2.Client {
	t.Helper()
	cfg, err := loadAWSConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("loadAWSConfig: %v", err)
	}
	return ec2.NewFromConfig(cfg)
}

func subnetsXML(pairs ...[2]string) string {
	var sb strings.Builder
	for _, p := range pairs {
		sb.WriteString(`<item><subnetId>` + p[0] + `</subnetId><cidrBlock>` + p[1] + `</cidrBlock></item>`)
	}
	return `<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <subnetSet>` + sb.String() + `</subnetSet>
</DescribeSubnetsResponse>`
}

// securityGroupsXML renders one group with a single TCP ingress rule that
// either references a source group or a CIDR.
func securityGroupsXML(groupID string, fromPort, toPort int, sourceGroup, cidr string) string {
	var src string
	if sourceGroup != "" {
		src = `<groups><item><groupId>` + sourceGroup + `</groupId></item></groups>`
	}
	if cidr != "" {
		src += `<ipRanges><item><cidrIp>` + cidr + `</cidrIp></item></ipRanges>`
	}
	return `<DescribeSecurityGroupsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <securityGroupInfo>
    <item>
      <groupId>` + groupID + `</groupId>
      <ipPermissions>
        <item>
          <ipProtocol>tcp</ipProtocol>
          <fromPort>` + itoa(fromPort) + `</fromPort>
          <toPort>` + itoa(toPort) + `</toPort>
          ` + src + `
        </item>
      </ipPermissions>
    </item>
  </securityGroupInfo>
</DescribeSecurityGroupsResponse>`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestSubnetCIDRs(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves ids to networks", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeSubnets",
			subnetsXML([2]string{"subnet-a", "10.0.1.0/24"}, [2]string{"subnet-b", "10.0.2.0/24"})))
		nets, err := subnetCIDRs(ctx, ec2Client(t), []string{"subnet-a", "subnet-b"})
		if err != nil {
			t.Fatalf("subnetCIDRs: %v", err)
		}
		if len(nets) != 2 || nets[0].String() != "10.0.1.0/24" {
			t.Errorf("nets = %v", nets)
		}
	})

	// No subnets means no call at all — an empty DescribeSubnets is an API error.
	t.Run("empty id list short-circuits", func(t *testing.T) {
		f := newAWSFake(t)
		stubAWS(t, f)
		nets, err := subnetCIDRs(ctx, ec2Client(t), nil)
		if err != nil || nets != nil {
			t.Fatalf("got (%v,%v)", nets, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("no ids should mean no AWS call, got %v", f.calls)
		}
	})

	t.Run("unparseable cidrs are skipped, not fatal", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeSubnets",
			subnetsXML([2]string{"subnet-a", "not-a-cidr"}, [2]string{"subnet-b", "10.0.2.0/24"})))
		nets, err := subnetCIDRs(ctx, ec2Client(t), []string{"subnet-a", "subnet-b"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(nets) != 1 || nets[0].String() != "10.0.2.0/24" {
			t.Errorf("nets = %v, want only the parseable one", nets)
		}
	})

	t.Run("api error is wrapped", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeSubnets", http.StatusForbidden,
			awsErrorXML("UnauthorizedOperation", "denied")))
		if _, err := subnetCIDRs(ctx, ec2Client(t), []string{"subnet-a"}); err == nil {
			t.Fatal("expected the API error")
		}
	})
}

func TestSecurityGroupIngress(t *testing.T) {
	ctx := context.Background()

	t.Run("keys permissions by group id", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeSecurityGroups",
			securityGroupsXML("sg-db", 5432, 5432, "sg-task", "")))
		got, err := securityGroupIngress(ctx, ec2Client(t), []string{"sg-db"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got["sg-db"]) != 1 {
			t.Fatalf("got %+v, want one permission for sg-db", got)
		}
		if aws.ToString(got["sg-db"][0].IpProtocol) != "tcp" {
			t.Errorf("protocol = %q", aws.ToString(got["sg-db"][0].IpProtocol))
		}
	})

	t.Run("empty id list short-circuits", func(t *testing.T) {
		f := newAWSFake(t)
		stubAWS(t, f)
		got, err := securityGroupIngress(ctx, ec2Client(t), nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got (%v,%v)", got, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("no ids should mean no AWS call, got %v", f.calls)
		}
	})

	t.Run("api error is wrapped with the ids", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeSecurityGroups", http.StatusForbidden,
			awsErrorXML("UnauthorizedOperation", "denied")))
		_, err := securityGroupIngress(ctx, ec2Client(t), []string{"sg-db"})
		if err == nil || !strings.Contains(err.Error(), "sg-db") {
			t.Fatalf("err = %v, want the group named", err)
		}
	})
}

func TestDistinctDBSGs(t *testing.T) {
	got := distinctDBSGs([]AwsTarget{
		{DBSecurityGroups: []string{"sg-a", "sg-b"}},
		{DBSecurityGroups: []string{"sg-b", ""}}, // duplicate + empty
		{DBSecurityGroups: nil},
		{DBSecurityGroups: []string{"sg-c"}},
	})
	if len(got) != 3 || got[0] != "sg-a" || got[1] != "sg-b" || got[2] != "sg-c" {
		t.Errorf("got %v, want [sg-a sg-b sg-c] — deduped, empties dropped, order kept", got)
	}
	if distinctDBSGs(nil) != nil {
		t.Error("no targets should yield no ids")
	}
}

func TestVerifyNetworkPath_EndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("group reference admits the collector", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).
			on("DescribeSubnets", subnetsXML([2]string{"subnet-a", "10.0.1.0/24"})).
			on("DescribeSecurityGroups", securityGroupsXML("sg-db", 5432, 5432, "sg-task", "")))

		findings, err := VerifyNetworkPath(ctx, "sg-task", []string{"subnet-a"}, []AwsTarget{
			{Name: "prod", Port: 5432, DBSecurityGroups: []string{"sg-db"}},
		})
		if err != nil {
			t.Fatalf("VerifyNetworkPath: %v", err)
		}
		if len(findings) != 1 || !findings[0].Reachable {
			t.Fatalf("findings = %+v, want reachable", findings)
		}
		if findings[0].Remediation != "" {
			t.Error("a reachable path needs no remediation")
		}
	})

	t.Run("blocked path carries a concrete fix", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).
			on("DescribeSubnets", subnetsXML([2]string{"subnet-a", "10.0.1.0/24"})).
			// Rule admits some other group on the wrong port.
			on("DescribeSecurityGroups", securityGroupsXML("sg-db", 3306, 3306, "sg-other", "")))

		findings, err := VerifyNetworkPath(ctx, "sg-task", []string{"subnet-a"}, []AwsTarget{
			{Name: "prod", Port: 5432, DBSecurityGroups: []string{"sg-db"}},
		})
		if err != nil {
			t.Fatalf("VerifyNetworkPath: %v", err)
		}
		if findings[0].Reachable {
			t.Fatal("a rule on the wrong port must not count as reachable")
		}
		if !strings.Contains(findings[0].Remediation, "sg-db") ||
			!strings.Contains(findings[0].Remediation, "5432") {
			t.Errorf("remediation should name the group and port, got %q", findings[0].Remediation)
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if _, err := VerifyNetworkPath(ctx, "sg-task", nil, nil); err == nil {
			t.Fatal("expected the credential error")
		}
	})

	t.Run("subnet lookup failure aborts", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeSubnets", http.StatusForbidden,
			awsErrorXML("UnauthorizedOperation", "denied")))
		if _, err := VerifyNetworkPath(ctx, "sg-task", []string{"subnet-a"}, nil); err == nil {
			t.Fatal("expected the subnet error")
		}
	})

	t.Run("security-group lookup failure aborts", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).
			on("DescribeSubnets", subnetsXML([2]string{"subnet-a", "10.0.1.0/24"})).
			fail("DescribeSecurityGroups", http.StatusForbidden, awsErrorXML("UnauthorizedOperation", "denied")))
		_, err := VerifyNetworkPath(ctx, "sg-task", []string{"subnet-a"}, []AwsTarget{
			{Name: "prod", DBSecurityGroups: []string{"sg-db"}},
		})
		if err == nil {
			t.Fatal("expected the security-group error")
		}
	})
}

// A CIDR rule admits the collector only when it covers the task's subnets. The
// containment test is the one place an off-by-one prefix silently opens or
// closes the path.
func TestCIDRCoversAny_PrefixBoundaries(t *testing.T) {
	mustNet := func(s string) *net.IPNet {
		n := parseCIDR(s)
		if n == nil {
			t.Fatalf("parseCIDR(%q) = nil", s)
		}
		return n
	}
	task := []*net.IPNet{mustNet("10.0.1.0/24")}

	if !cidrCoversAny(mustNet("10.0.0.0/16"), task) {
		t.Error("a /16 containing the subnet must admit it")
	}
	if !cidrCoversAny(mustNet("10.0.1.0/24"), task) {
		t.Error("an exactly-matching CIDR must admit it")
	}
	// More specific than the subnet: covers only part of it, so it must not
	// count as admitting the whole task range.
	if cidrCoversAny(mustNet("10.0.1.0/25"), task) {
		t.Error("a /25 does not cover the whole /24")
	}
	if cidrCoversAny(mustNet("192.168.0.0/16"), task) {
		t.Error("an unrelated range must not admit the task")
	}
	if cidrCoversAny(mustNet("10.0.0.0/8"), []*net.IPNet{nil}) {
		t.Error("a nil inner network must be skipped, not matched")
	}
	if cidrCoversAny(mustNet("10.0.0.0/8"), nil) {
		t.Error("no task subnets means nothing is covered")
	}
}

func TestParseCIDR_BadInputIsNilNotFatal(t *testing.T) {
	for _, s := range []string{"", "not-a-cidr", "10.0.0.0", "10.0.0.0/99"} {
		if got := parseCIDR(s); got != nil {
			t.Errorf("parseCIDR(%q) = %v, want nil", s, got)
		}
	}
	if got := parseCIDR("2001:db8::/32"); got == nil {
		t.Error("a valid IPv6 CIDR should still parse")
	}
}
