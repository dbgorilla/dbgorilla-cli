package collector

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Network-path verification. The collector runs as a Fargate task inside the
// VPC; the CLI runs outside it and usually can't reach a private RDS to probe
// live. So instead of a TCP dial we read the VPC's security-group rules and
// answer statically: would the collector's task (its security group, in its
// subnets) be admitted to the database on its port? This catches the common
// "deployed but silently can't connect" case — a DB whose security group
// doesn't allow the collector — before the stack is even created.

// NetworkFinding is the reachability verdict for one monitored database.
type NetworkFinding struct {
	Target    string // component / instance name, for display
	Port      int
	Reachable bool
	// Detail explains the verdict: how the path is allowed (a group reference or
	// a CIDR), or — when blocked/unknown — why, so it can front a remediation.
	Detail string
	// Remediation, when not reachable, is the concrete fix (the ingress rule to
	// add). Empty when reachable or when we couldn't determine the DB's group.
	Remediation string
}

// VerifyNetworkPath checks, per target, whether the collector task (running in
// taskSubnets with security group taskSG) is admitted to the database on its
// port by the database's security-group ingress rules. It is a static analysis
// of VPC rules — no live probe — so it works for private databases the CLI
// can't reach. A target whose DB security group we couldn't determine comes
// back Reachable=false with an explanatory Detail and no Remediation (unknown,
// not blocked).
func VerifyNetworkPath(ctx context.Context, taskSG string, taskSubnets []string, targets []AwsTarget) ([]NetworkFinding, error) {
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	client := ec2.NewFromConfig(cfg)

	// The collector's task IPs come from its subnets, so an ingress CIDR that
	// covers a task subnet admits the task. Resolve those subnet CIDRs once.
	taskCIDRs, err := subnetCIDRs(ctx, client, taskSubnets)
	if err != nil {
		return nil, err
	}

	// Batch-fetch the ingress rules of every distinct DB security group.
	ingress, err := securityGroupIngress(ctx, client, distinctDBSGs(targets))
	if err != nil {
		return nil, err
	}

	findings := make([]NetworkFinding, 0, len(targets))
	for _, t := range targets {
		findings = append(findings, evaluateReachability(t, taskSG, taskCIDRs, ingress))
	}
	return findings, nil
}

// evaluateReachability decides one target's verdict by asking whether any of the
// database's own security groups admits the collector's task (its group, or its
// subnets' IPs) on the DB port. Pure, so it's unit-testable without EC2.
func evaluateReachability(t AwsTarget, taskSG string, taskCIDRs []*net.IPNet, ingress map[string][]ec2types.IpPermission) NetworkFinding {
	port := t.Port
	if port == 0 {
		port = 5432
	}
	f := NetworkFinding{Target: orDefault(t.Name, t.InstanceID), Port: port}

	if len(t.DBSecurityGroups) == 0 {
		f.Detail = "the database's security groups weren't discovered, so the network path can't be verified here"
		return f
	}
	for _, dbSG := range t.DBSecurityGroups {
		for _, p := range ingress[dbSG] {
			if !permCoversPort(p, port) {
				continue
			}
			// A rule that references the collector's own security group admits it.
			for _, pair := range p.UserIdGroupPairs {
				if aws.ToString(pair.GroupId) == taskSG {
					f.Reachable = true
					if taskSG == dbSG {
						f.Detail = fmt.Sprintf("security group %s admits itself on port %d (collector shares the database's group)", taskSG, port)
					} else {
						f.Detail = fmt.Sprintf("database security group %s admits the collector's group %s on port %d", dbSG, taskSG, port)
					}
					return f
				}
			}
			// A rule whose CIDR covers a collector subnet admits the task's IP.
			for _, r := range p.IpRanges {
				cidr := aws.ToString(r.CidrIp)
				if outer := parseCIDR(cidr); outer != nil && cidrCoversAny(outer, taskCIDRs) {
					f.Reachable = true
					f.Detail = fmt.Sprintf("database security group %s admits %s on port %d, which covers the collector's subnets", dbSG, cidr, port)
					return f
				}
			}
		}
	}

	// No admitting rule on any of the DB's groups: report the gap and the fix.
	f.Detail = fmt.Sprintf("none of the database's security groups (%s) admit the collector on port %d", strings.Join(t.DBSecurityGroups, ", "), port)
	src := taskSG
	if src == "" {
		src = "<collector-security-group>"
	}
	f.Remediation = fmt.Sprintf("add an inbound rule to %s: TCP port %d from source security group %s", t.DBSecurityGroups[0], port, src)
	return f
}

// permCoversPort reports whether a security-group permission's protocol and port
// range include the given TCP port. Protocol "-1" (all) or "tcp"/"6" qualifies;
// a nil FromPort/ToPort (all ports, as with -1) counts as covering.
func permCoversPort(p ec2types.IpPermission, port int) bool {
	proto := aws.ToString(p.IpProtocol)
	if proto != "-1" && proto != "tcp" && proto != "6" {
		return false
	}
	if p.FromPort == nil || p.ToPort == nil {
		return true // e.g. protocol -1 with no port bounds = all ports
	}
	return int(*p.FromPort) <= port && port <= int(*p.ToPort)
}

// distinctDBSGs collects the unique DB security-group ids across all targets, so
// we DescribeSecurityGroups once.
func distinctDBSGs(targets []AwsTarget) []string {
	seen := map[string]bool{}
	var ids []string
	for _, t := range targets {
		for _, sg := range t.DBSecurityGroups {
			if sg != "" && !seen[sg] {
				seen[sg] = true
				ids = append(ids, sg)
			}
		}
	}
	return ids
}

// securityGroupIngress returns each security group's ingress permissions, keyed
// by group id. An empty id list short-circuits (no call).
func securityGroupIngress(ctx context.Context, client *ec2.Client, ids []string) (map[string][]ec2types.IpPermission, error) {
	out := map[string][]ec2types.IpPermission{}
	if len(ids) == 0 {
		return out, nil
	}
	resp, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: ids})
	if err != nil {
		return nil, fmt.Errorf("could not read security groups %v: %w", ids, err)
	}
	for _, g := range resp.SecurityGroups {
		out[aws.ToString(g.GroupId)] = g.IpPermissions
	}
	return out, nil
}

// subnetCIDRs resolves subnet ids to their IPv4 CIDR blocks. An empty id list
// short-circuits.
func subnetCIDRs(ctx context.Context, client *ec2.Client, ids []string) ([]*net.IPNet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	resp, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: ids})
	if err != nil {
		return nil, fmt.Errorf("could not read subnets %v: %w", ids, err)
	}
	var nets []*net.IPNet
	for _, s := range resp.Subnets {
		if n := parseCIDR(aws.ToString(s.CidrBlock)); n != nil {
			nets = append(nets, n)
		}
	}
	return nets, nil
}

// cidrCoversAny reports whether outer fully contains any of the inner networks
// (inner ⊆ outer): outer holds inner's base address and is no more specific.
func cidrCoversAny(outer *net.IPNet, inners []*net.IPNet) bool {
	for _, inner := range inners {
		if inner == nil {
			continue
		}
		outerOnes, _ := outer.Mask.Size()
		innerOnes, _ := inner.Mask.Size()
		if outerOnes <= innerOnes && outer.Contains(inner.IP) {
			return true
		}
	}
	return false
}

// parseCIDR parses a CIDR, returning nil (not an error) on anything unparseable
// so callers can skip odd entries (e.g. IPv6 or malformed rules) gracefully.
func parseCIDR(s string) *net.IPNet {
	if s == "" {
		return nil
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return nil
	}
	return n
}
