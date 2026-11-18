package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ingressRuleMarker is the description on every database rule this CLI adds.
// A rule that carries it and admits the collector's own group was made by an
// earlier run, so it is claimed rather than left unrecorded.
const ingressRuleMarker = "DBGorilla collector (managed by the dbgorilla CLI)"

// DBIngressRule is one rule the install added to a database's security group so
// the collector's own group can reach it. Uninstall revokes exactly these.
type DBIngressRule struct {
	GroupID string `json:"group_id"`
	RuleID  string `json:"rule_id"`
}

// DBIngressNeed is one database security group and port the collector must be
// admitted to.
type DBIngressNeed struct {
	GroupID string
	Port    int32
}

// DBIngressNeeds lists, without repeats, each database security group and port
// the collector connects to.
func DBIngressNeeds(targets []AwsTarget) []DBIngressNeed {
	seen := map[DBIngressNeed]bool{}
	var out []DBIngressNeed
	for _, t := range targets {
		for _, g := range t.DBSecurityGroups {
			n := DBIngressNeed{GroupID: g, Port: int32(t.Port)}
			if g == "" || t.Port == 0 || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// CollectorEgressPortsFor is the gateway port plus every database port.
func CollectorEgressPortsFor(targets []AwsTarget) []int32 {
	ports := []int32{443}
	seen := map[int32]bool{443: true}
	for _, t := range targets {
		p := int32(t.Port)
		if p != 0 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	return ports
}

// SubnetsVPC is the VPC the given subnets belong to. They must share one.
func SubnetsVPC(ctx context.Context, client *ec2.Client, subnets []string) (string, error) {
	out, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: subnets})
	if err != nil {
		return "", fmt.Errorf("cannot look up the VPC of subnets %v: %w. Check --subnets and that your credentials allow ec2:DescribeSubnets", subnets, err)
	}
	vpc := ""
	for _, s := range out.Subnets {
		v := aws.ToString(s.VpcId)
		if vpc != "" && v != vpc {
			return "", fmt.Errorf("subnets %v span more than one VPC; pass --subnets from the databases' VPC only", subnets)
		}
		vpc = v
	}
	if vpc == "" {
		return "", fmt.Errorf("subnets %v were not found in this region; check --subnets and the region", subnets)
	}
	return vpc, nil
}

// EnsureDBIngress admits collectorSG to dbSG on port. It returns the rule it
// created. When an identical rule already exists, it returns that rule's id if
// this CLI made it (an earlier run that failed before recording it), and an
// empty id otherwise, so uninstall leaves someone else's rule alone.
func EnsureDBIngress(ctx context.Context, client *ec2.Client, dbSG string, port int32, collectorSG string) (string, error) {
	out, err := client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(dbSG),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(port),
			ToPort:     aws.Int32(port),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{{
				GroupId:     aws.String(collectorSG),
				Description: aws.String(ingressRuleMarker),
			}},
		}},
	})
	if awsErrorCodeIs(err, "InvalidPermission.Duplicate") {
		return ourExistingIngress(ctx, client, dbSG, port, collectorSG), nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot admit the collector to security group %s on port %d: %w", dbSG, port, err)
	}
	for _, r := range out.SecurityGroupRules {
		return aws.ToString(r.SecurityGroupRuleId), nil
	}
	return "", nil
}

// ourExistingIngress finds the rule on dbSG that admits collectorSG on port
// with this CLI's description. A failed lookup counts as "not ours": leaving a
// rule unrecorded is the old behaviour, and never claims someone else's.
func ourExistingIngress(ctx context.Context, client *ec2.Client, dbSG string, port int32, collectorSG string) string {
	out, err := client.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{dbSG}}},
	})
	if err != nil {
		return ""
	}
	for _, r := range out.SecurityGroupRules {
		if aws.ToBool(r.IsEgress) || aws.ToString(r.IpProtocol) != "tcp" ||
			aws.ToInt32(r.FromPort) != port || aws.ToInt32(r.ToPort) != port ||
			r.ReferencedGroupInfo == nil || aws.ToString(r.ReferencedGroupInfo.GroupId) != collectorSG ||
			aws.ToString(r.Description) != ingressRuleMarker {
			continue
		}
		return aws.ToString(r.SecurityGroupRuleId)
	}
	return ""
}

// RevokeDBIngress removes the rules the install added. A rule already gone is
// not an error. Every rule is attempted; the first failure is returned.
func RevokeDBIngress(ctx context.Context, client *ec2.Client, rules []DBIngressRule) error {
	var first error
	for _, r := range rules {
		if r.RuleID == "" {
			continue
		}
		_, err := client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:              aws.String(r.GroupID),
			SecurityGroupRuleIds: []string{r.RuleID},
		})
		if err != nil && !awsErrorCodeIs(err, "InvalidSecurityGroupRuleId.NotFound") &&
			!isMissingSGError(err) && first == nil {
			first = fmt.Errorf("cannot remove rule %s from security group %s: %w", r.RuleID, r.GroupID, err)
		}
	}
	return first
}

// CollectorNetwork is the collector's own security group and the database
// rules that admit it.
type CollectorNetwork struct {
	SecurityGroupID      string
	SecurityGroupCreated bool
	Rules                []DBIngressRule
}

// EnsureCollectorNetwork gives the collector its own security group in the
// subnets' VPC (no ingress, egress only to 443 and the database ports), and
// admits that group to each database's security group on the database port.
// On a failure part way, whatever it created is removed again.
func EnsureCollectorNetwork(ctx context.Context, region, stackName string, subnets []string, targets []AwsTarget) (CollectorNetwork, error) {
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return CollectorNetwork{}, err
	}
	client := ec2.NewFromConfig(cfg)
	vpc, err := SubnetsVPC(ctx, client, subnets)
	if err != nil {
		return CollectorNetwork{}, err
	}
	id, created, err := ensureCollectorSecurityGroupWithPorts(ctx, client, vpc, stackName, CollectorEgressPortsFor(targets), true)
	if err != nil {
		return CollectorNetwork{}, err
	}
	net := CollectorNetwork{SecurityGroupID: id, SecurityGroupCreated: created}
	for _, n := range DBIngressNeeds(targets) {
		rule, err := EnsureDBIngress(ctx, client, n.GroupID, n.Port, id)
		if err != nil {
			if rerr := ReleaseCollectorNetwork(ctx, region, net); rerr != nil {
				return CollectorNetwork{}, fmt.Errorf("%w; undoing the network changes also failed (%v). Remove them with:\n%s",
					err, rerr, ManualNetworkCleanup(region, net))
			}
			return CollectorNetwork{}, err
		}
		if rule != "" {
			net.Rules = append(net.Rules, DBIngressRule{GroupID: n.GroupID, RuleID: rule})
		}
	}
	return net, nil
}

// ReleaseCollectorNetwork revokes the database rules first (EC2 will not
// delete a group another group's rule refers to), then the group if this
// install created it, retrying for a bounded time while it is still in use.
func ReleaseCollectorNetwork(ctx context.Context, region string, net CollectorNetwork) error {
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)
	if err := RevokeDBIngress(ctx, client, net.Rules); err != nil {
		return err
	}
	if !net.SecurityGroupCreated {
		return nil
	}
	return deleteCollectorSecurityGroupWhenFree(ctx, client, net.SecurityGroupID)
}

// ManualNetworkCleanup is the aws CLI commands that remove what net holds:
// the database rules first, then the group if this install created it.
func ManualNetworkCleanup(region string, net CollectorNetwork) string {
	var b strings.Builder
	for _, r := range net.Rules {
		fmt.Fprintf(&b, "  aws ec2 revoke-security-group-ingress --group-id %s --security-group-rule-ids %s --region %s\n",
			r.GroupID, r.RuleID, region)
	}
	if net.SecurityGroupCreated && net.SecurityGroupID != "" {
		fmt.Fprintf(&b, "  aws ec2 delete-security-group --group-id %s --region %s\n", net.SecurityGroupID, region)
	}
	return strings.TrimRight(b.String(), "\n")
}

// AdmitCollector is the update-path half of EnsureCollectorNetwork: the group
// already exists, and a database added since install needs its port opened in
// the group's egress and the group admitted to its security group. Returns the
// rules it created.
func AdmitCollector(ctx context.Context, region, collectorSG string, targets []AwsTarget) ([]DBIngressRule, error) {
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return nil, err
	}
	client := ec2.NewFromConfig(cfg)
	for _, port := range CollectorEgressPortsFor(targets) {
		if err := authorizeEgress(ctx, client, collectorSG, []int32{port}); err != nil &&
			!awsErrorCodeIs(err, "InvalidPermission.Duplicate") {
			return nil, err
		}
	}
	var rules []DBIngressRule
	for _, n := range DBIngressNeeds(targets) {
		rule, err := EnsureDBIngress(ctx, client, n.GroupID, n.Port, collectorSG)
		if err != nil {
			return rules, err
		}
		if rule != "" {
			rules = append(rules, DBIngressRule{GroupID: n.GroupID, RuleID: rule})
		}
	}
	return rules, nil
}
