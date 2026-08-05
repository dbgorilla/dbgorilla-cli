package collector

import (
	"net"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func tcp(from, to int32, groups []string, cidrs []string) ec2types.IpPermission {
	p := ec2types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
	for _, g := range groups {
		p.UserIdGroupPairs = append(p.UserIdGroupPairs, ec2types.UserIdGroupPair{GroupId: aws.String(g)})
	}
	for _, c := range cidrs {
		p.IpRanges = append(p.IpRanges, ec2types.IpRange{CidrIp: aws.String(c)})
	}
	return p
}

func mustCIDRs(t *testing.T, ss ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("bad test CIDR %q: %v", s, err)
		}
		out = append(out, n)
	}
	return out
}

func TestEvaluateReachability(t *testing.T) {
	target := AwsTarget{Name: "prod", InstanceID: "prod", Port: 5432, DBSecurityGroups: []string{"sg-db"}}

	cases := []struct {
		name      string
		taskSG    string
		taskCIDRs []*net.IPNet
		ingress   map[string][]ec2types.IpPermission
		reachable bool
		wantFix   bool
	}{
		{
			name:      "self-referencing SG on the DB port",
			taskSG:    "sg-db", // collector shares the DB's group
			ingress:   map[string][]ec2types.IpPermission{"sg-db": {tcp(5432, 5432, []string{"sg-db"}, nil)}},
			reachable: true,
		},
		{
			name:      "DB group admits the collector's distinct group",
			taskSG:    "sg-task",
			ingress:   map[string][]ec2types.IpPermission{"sg-db": {tcp(5432, 5432, []string{"sg-task"}, nil)}},
			reachable: true,
		},
		{
			name:      "CIDR covers the collector subnet",
			taskSG:    "sg-task",
			taskCIDRs: mustCIDRs(t, "10.0.1.0/24"),
			ingress:   map[string][]ec2types.IpPermission{"sg-db": {tcp(5432, 5432, nil, []string{"10.0.0.0/16"})}},
			reachable: true,
		},
		{
			name:      "no admitting rule -> blocked with a fix",
			taskSG:    "sg-task",
			taskCIDRs: mustCIDRs(t, "10.0.1.0/24"),
			ingress:   map[string][]ec2types.IpPermission{"sg-db": {tcp(5432, 5432, []string{"sg-other"}, []string{"192.168.0.0/16"})}},
			reachable: false,
			wantFix:   true,
		},
		{
			name:      "rule on a different port doesn't count",
			taskSG:    "sg-db",
			ingress:   map[string][]ec2types.IpPermission{"sg-db": {tcp(443, 443, []string{"sg-db"}, nil)}},
			reachable: false,
			wantFix:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := evaluateReachability(target, c.taskSG, c.taskCIDRs, c.ingress)
			if f.Reachable != c.reachable {
				t.Fatalf("reachable = %v, want %v (detail: %s)", f.Reachable, c.reachable, f.Detail)
			}
			if (f.Remediation != "") != c.wantFix {
				t.Errorf("remediation = %q, wantFix = %v", f.Remediation, c.wantFix)
			}
		})
	}
}

func TestEvaluateReachability_UnknownWhenNoDBGroup(t *testing.T) {
	f := evaluateReachability(AwsTarget{Name: "prod", Port: 5432}, "sg-task", nil, nil)
	if f.Reachable {
		t.Error("a target with no DB security group can't be reachable")
	}
	if f.Remediation != "" {
		t.Error("unknown (not blocked) should carry no remediation")
	}
}

func TestPermCoversPort(t *testing.T) {
	if !permCoversPort(tcp(5000, 6000, nil, nil), 5432) {
		t.Error("range 5000-6000 should cover 5432")
	}
	allProto := ec2types.IpPermission{IpProtocol: aws.String("-1")}
	if !permCoversPort(allProto, 5432) {
		t.Error("protocol -1 (all) with no bounds should cover any port")
	}
	udp := ec2types.IpPermission{IpProtocol: aws.String("udp"), FromPort: aws.Int32(5432), ToPort: aws.Int32(5432)}
	if permCoversPort(udp, 5432) {
		t.Error("udp should not count for a TCP database port")
	}
}

func TestCIDRCoversAny(t *testing.T) {
	_, outer, _ := net.ParseCIDR("10.0.0.0/16")
	if !cidrCoversAny(outer, mustCIDRs(t, "10.0.5.0/24")) {
		t.Error("10.0.0.0/16 should cover 10.0.5.0/24")
	}
	if cidrCoversAny(outer, mustCIDRs(t, "10.1.0.0/24")) {
		t.Error("10.0.0.0/16 should not cover 10.1.0.0/24")
	}
	// A more-specific 'outer' cannot contain a broader inner.
	_, narrow, _ := net.ParseCIDR("10.0.5.0/24")
	if cidrCoversAny(narrow, mustCIDRs(t, "10.0.0.0/16")) {
		t.Error("/24 cannot cover a /16")
	}
}
