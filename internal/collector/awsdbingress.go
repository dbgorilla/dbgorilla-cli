package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

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
		return "", fmt.Errorf("cannot look up the VPC of subnets %v: %w", subnets, err)
	}
	vpc := ""
	for _, s := range out.Subnets {
		v := aws.ToString(s.VpcId)
		if vpc != "" && v != vpc {
			return "", fmt.Errorf("subnets %v span more than one VPC", subnets)
		}
		vpc = v
	}
	if vpc == "" {
		return "", errors.New("the collector's subnets were not found")
	}
	return vpc, nil
}

// EnsureDBIngress admits collectorSG to dbSG on port. It returns the rule it
// created, or an empty id when an identical rule already existed (so uninstall
// leaves that one alone).
func EnsureDBIngress(ctx context.Context, client *ec2.Client, dbSG string, port int32, collectorSG string) (string, error) {
	out, err := client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(dbSG),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(port),
			ToPort:     aws.Int32(port),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{{
				GroupId:     aws.String(collectorSG),
				Description: aws.String("DBGorilla collector (managed by the dbgorilla CLI)"),
			}},
		}},
	})
	if awsErrorCodeIs(err, "InvalidPermission.Duplicate") {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot admit the collector to security group %s on port %d: %w", dbSG, port, err)
	}
	for _, r := range out.SecurityGroupRules {
		return aws.ToString(r.SecurityGroupRuleId), nil
	}
	return "", nil
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
			_ = ReleaseCollectorNetwork(ctx, region, net)
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
// install created it.
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
	return DeleteCollectorSecurityGroup(ctx, client, net.SecurityGroupID)
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
