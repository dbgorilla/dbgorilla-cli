package collector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/jackc/pgx/v5"
)

// The CloudFormation template that defines the collector's AWS deployment — a
// single Fargate task (the collector is a stateless singleton) plus its ECS
// cluster, task/execution IAM roles, and a Secrets Manager secret for the OpAMP
// client secret — lives in cloudformation/collector-fargate.yaml and is
// published to S3 per release. It is deliberately NOT embedded in this binary:
// the published copy is the single source of truth, so what a customer reviews
// at the URL is exactly what gets deployed, and there is no second copy that
// could drift from it or be deployed unverified. See awstemplate.go.

// DefaultStackName is the CloudFormation stack the AWS target creates/updates.
const DefaultStackName = "dbgorilla-collector"

// AwsAvailable, AwsIdentity, and AwsRegion now live in awsclient.go (aws-sdk-go-v2).

// DefaultDBUser is the IAM database user the collector authenticates as when the
// caller does not specify one. It must exist in the database with rds_iam
// granted; the CLI prints that GRANT after a successful deploy.
const DefaultDBUser = "dbgorilla"

// AwsTarget is the resolved RDS database + VPC networking the Fargate collector
// monitors. Explicitly-provided fields win; the rest are auto-discovered from
// the RDS instance.
type AwsTarget struct {
	Name          string // display name for the component in DBGorilla
	InstanceID    string // RDS DBInstanceIdentifier
	DbiResourceID string // RDS DbiResourceId — scopes rds-db:connect (least privilege)
	Host          string // RDS endpoint address
	Port          int
	User          string   // IAM-enabled database user
	Databases     []string // empty => the instance's default database
	SSLMode       string
	ProviderType  string // aws_rds | aws_aurora
	AuthMethod    string // "iam" (default) | "password" — password rides Secrets Manager
	// Commands are the query-analysis commands this database allows the collector
	// to run (execute_query, explain), clamped to the engine. Empty means none
	// (query analysis off for this component).
	Commands      []string
	Subnets       []string
	SecurityGroup string // the security group the collector task runs in (defaults to the DB's)
	// DBSecurityGroups are the database's own security groups, captured at
	// discovery for the network-path check (whether the task can reach the DB).
	// Distinct from SecurityGroup, which — via --security-group-id — may be a
	// different group the task runs in. Empty when the target skipped discovery.
	DBSecurityGroups []string
	IAMAuthOn        bool // whether the instance has IAM auth enabled (warn if not)
}

// rdsInstance is the subset of an RDS instance the discovery reads, mapped from
// the SDK's DescribeDBInstances output into a plain struct the merge/tests use.
type rdsInstance struct {
	ID             string
	Engine         string
	DbiResourceID  string
	DBName         string
	IAMAuthEnabled bool
	Host           string
	Port           int
	Subnets        []string
	SecurityGroups []sgMembership
}

// sgMembership is a VPC security group attached to a database and its status.
type sgMembership struct {
	ID     string
	Status string
}

// ErrUnsupportedEngine rejects a database this CLI cannot deploy a collector
// for. The aws target is PostgreSQL-only end to end: awsComponent emits
// engine = "postgres", and the grant helpers speak PostgreSQL (rds_iam /
// pg_monitor over a postgres:// admin connection on 5432). The collector itself
// supports MySQL, so a hand-written config can still drive it — but discovery
// must not hand a MySQL database to this path and label it postgres.
var ErrUnsupportedEngine = errors.New("unsupported database engine")

// supportedEngine reports whether an RDS engine string is one this CLI handles.
// RDS reports "postgres" for instances and "aurora-postgresql" for clusters.
func supportedEngine(engine string) bool {
	return strings.HasPrefix(engine, "postgres") || strings.HasPrefix(engine, "aurora-postgresql")
}

// checkEngine fails closed on a database this CLI cannot configure correctly.
func checkEngine(engine, id string) error {
	if supportedEngine(engine) {
		return nil
	}
	return fmt.Errorf("%w: %s runs %s, but the AWS collector target supports PostgreSQL only. "+
		"Pick a PostgreSQL database with --db-instance-id, or configure this one by hand "+
		"(see examples/collector-aws.toml) and deploy with --config",
		ErrUnsupportedEngine, id, engine)
}

// DiscoverAwsTarget resolves the database to monitor — a standalone RDS instance
// (aws_rds) or an Aurora cluster (aws_aurora) — into an AwsTarget. With id empty
// it auto-selects the sole Postgres instance/cluster and refuses to guess when
// ambiguous (curl|bash-style non-interactive: stop and ask rather than pick
// wrong). providerHint ("aws_rds"/"aws_aurora"/"") short-circuits the
// instance-vs-cluster lookup; empty tries an instance, then a cluster.
// Discovered fields fill only what the AwsTarget leaves unset — explicit wins.
func DiscoverAwsTarget(id, providerHint string, into AwsTarget) (AwsTarget, error) {
	if id == "" {
		soloID, kind, err := soloTarget()
		if err != nil {
			return into, err
		}
		id, providerHint = soloID, kind
	}

	switch providerHint {
	case "aws_aurora":
		return discoverCluster(id, into)
	case "aws_rds":
		return discoverInstance(id, into)
	default:
		// Explicit id, no hint: try an instance, fall back to a cluster. A wrong
		// engine is a definite answer about a database that does exist, so report
		// it rather than falling through to the generic "not found".
		inst, err := describeInstance(id)
		if err == nil {
			return mergeInstance(into, inst), nil
		}
		if errors.Is(err, ErrUnsupportedEngine) {
			return into, err
		}
		if t, cerr := discoverCluster(id, into); cerr == nil {
			return t, nil
		} else if errors.Is(cerr, ErrUnsupportedEngine) {
			return into, cerr
		}
		return into, fmt.Errorf("no RDS instance or Aurora cluster named %q found in this region — "+
			"check the AWS region (AWS_REGION / the active profile) and pass an existing "+
			"--db-instance-id, then re-run", id)
	}
}

func discoverInstance(id string, into AwsTarget) (AwsTarget, error) {
	inst, err := describeInstance(id)
	if err != nil {
		return into, err
	}
	return mergeInstance(into, inst), nil
}

func discoverCluster(id string, into AwsTarget) (AwsTarget, error) {
	c, err := describeCluster(id)
	if err != nil {
		return into, err
	}
	subnets, err := subnetGroupSubnets(c.SubnetGroup)
	if err != nil {
		return into, err
	}
	return mergeCluster(into, c, subnets), nil
}

// mergeInstance fills an AwsTarget's unset fields from a described RDS instance;
// explicitly-set fields are preserved. Pure (no AWS calls) so it's unit-testable.
func mergeInstance(into AwsTarget, inst *rdsInstance) AwsTarget {
	t := into
	t.InstanceID = orDefault(t.InstanceID, inst.ID)
	t.DbiResourceID = orDefault(t.DbiResourceID, inst.DbiResourceID)
	t.Host = orDefault(t.Host, inst.Host)
	if t.Port == 0 {
		t.Port = inst.Port
	}
	t.Name = orDefault(t.Name, inst.ID)
	t.User = orDefault(t.User, DefaultDBUser)
	t.SSLMode = orDefault(t.SSLMode, "verify-full")
	t.ProviderType = orDefault(t.ProviderType, "aws_rds")
	if len(t.Databases) == 0 && inst.DBName != "" {
		t.Databases = []string{inst.DBName}
	}
	if len(t.Subnets) == 0 {
		t.Subnets = append(t.Subnets, inst.Subnets...)
	}
	if t.SecurityGroup == "" {
		t.SecurityGroup = firstActiveSG(inst.SecurityGroups)
	}
	t.DBSecurityGroups = activeSGs(inst.SecurityGroups)
	t.IAMAuthOn = inst.IAMAuthEnabled
	return t
}

// firstActiveSG returns the id of the first active security group, or "".
func firstActiveSG(sgs []sgMembership) string {
	for _, g := range sgs {
		if g.Status == "active" {
			return g.ID
		}
	}
	return ""
}

// activeSGs returns the ids of every active security group, in order.
func activeSGs(sgs []sgMembership) []string {
	var ids []string
	for _, g := range sgs {
		if g.Status == "active" {
			ids = append(ids, g.ID)
		}
	}
	return ids
}

// rdsCluster is the subset of `aws rds describe-db-clusters` we read. Unlike an
// instance, the cluster's Endpoint is a bare host string and subnets come from
// its named subnet group (a second lookup).
type rdsCluster struct {
	ID             string
	Engine         string
	Host           string // writer endpoint host
	Port           int
	ResourceID     string // DbClusterResourceId
	DatabaseName   string
	IAMAuthEnabled bool
	SubnetGroup    string // subnet group name (a second lookup resolves its subnets)
	SecurityGroups []sgMembership
}

// describeCluster reads one Aurora cluster's details.
func describeCluster(id string) (*rdsCluster, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	out, err := rds.NewFromConfig(cfg).DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(id),
	})
	if err != nil {
		return nil, fmt.Errorf("could not describe Aurora cluster %q: %w", id, err)
	}
	if len(out.DBClusters) == 0 {
		return nil, fmt.Errorf("no Aurora cluster named %q found in this region — check the AWS region "+
			"(AWS_REGION / the active profile) and pass an existing --db-instance-id, then re-run", id)
	}
	cl := clusterFromSDK(out.DBClusters[0])
	if err := checkEngine(cl.Engine, id); err != nil {
		return nil, err
	}
	return cl, nil
}

// clusterFromSDK maps an SDK DBCluster into our plain intermediate.
func clusterFromSDK(in rdstypes.DBCluster) *rdsCluster {
	c := &rdsCluster{
		ID:             aws.ToString(in.DBClusterIdentifier),
		Engine:         aws.ToString(in.Engine),
		Host:           aws.ToString(in.Endpoint),
		Port:           int(aws.ToInt32(in.Port)),
		ResourceID:     aws.ToString(in.DbClusterResourceId),
		DatabaseName:   aws.ToString(in.DatabaseName),
		IAMAuthEnabled: aws.ToBool(in.IAMDatabaseAuthenticationEnabled),
		SubnetGroup:    aws.ToString(in.DBSubnetGroup),
	}
	for _, g := range in.VpcSecurityGroups {
		c.SecurityGroups = append(c.SecurityGroups, sgMembership{ID: aws.ToString(g.VpcSecurityGroupId), Status: aws.ToString(g.Status)})
	}
	return c
}

// subnetGroupSubnets resolves a DB subnet group name to its subnet IDs (clusters
// reference a subnet group by name, not the subnet IDs directly).
func subnetGroupSubnets(name string) ([]string, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	out, err := rds.NewFromConfig(cfg).DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{
		DBSubnetGroupName: aws.String(name),
	})
	if err != nil {
		return nil, fmt.Errorf("could not resolve subnet group %q: %w", name, err)
	}
	var subnets []string
	if len(out.DBSubnetGroups) > 0 {
		for _, sn := range out.DBSubnetGroups[0].Subnets {
			subnets = append(subnets, aws.ToString(sn.SubnetIdentifier))
		}
	}
	return subnets, nil
}

// mergeCluster fills an AwsTarget's unset fields from a described Aurora cluster;
// explicitly-set fields are preserved. ProviderType is forced to aws_aurora, and
// the identifier/resource-id are the cluster's (the collector's aws_aurora
// provider takes a cluster_id; rds-db:connect scopes to the cluster resource id).
func mergeCluster(into AwsTarget, c *rdsCluster, subnets []string) AwsTarget {
	t := into
	t.InstanceID = orDefault(t.InstanceID, c.ID)
	t.DbiResourceID = orDefault(t.DbiResourceID, c.ResourceID)
	t.Host = orDefault(t.Host, c.Host)
	if t.Port == 0 {
		t.Port = c.Port
	}
	t.Name = orDefault(t.Name, c.ID)
	t.User = orDefault(t.User, DefaultDBUser)
	t.SSLMode = orDefault(t.SSLMode, "verify-full")
	t.ProviderType = "aws_aurora"
	if len(t.Databases) == 0 && c.DatabaseName != "" {
		t.Databases = []string{c.DatabaseName}
	}
	if len(t.Subnets) == 0 {
		t.Subnets = subnets
	}
	if t.SecurityGroup == "" {
		t.SecurityGroup = firstActiveSG(c.SecurityGroups)
	}
	t.DBSecurityGroups = activeSGs(c.SecurityGroups)
	t.IAMAuthOn = c.IAMAuthEnabled
	return t
}

// soloTarget auto-selects the single monitorable database — a Postgres RDS
// instance or an Aurora cluster — returning its id and provider kind. Errors on
// none or more than one (never guesses; the caller passes --db-instance-id to
// disambiguate).
func soloTarget() (id, kind string, err error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return "", "", err
	}
	client := rds.NewFromConfig(cfg)

	instOut, err := client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{})
	if err != nil {
		return "", "", fmt.Errorf("could not list RDS instances (check AWS permissions): %w", err)
	}
	var instances []string
	for _, in := range instOut.DBInstances {
		if strings.HasPrefix(aws.ToString(in.Engine), "postgres") {
			instances = append(instances, aws.ToString(in.DBInstanceIdentifier))
		}
	}

	clOut, err := client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{})
	if err != nil {
		return "", "", fmt.Errorf("could not list Aurora clusters: %w", err)
	}
	var clusters []string
	for _, cl := range clOut.DBClusters {
		if aws.ToString(cl.Engine) == "aurora-postgresql" {
			clusters = append(clusters, aws.ToString(cl.DBClusterIdentifier))
		}
	}
	return selectSolo(instances, clusters)
}

// TargetChoice is one candidate database the auto-selector found: its id and
// which provider (aws_rds / aws_aurora) it belongs to.
type TargetChoice struct {
	ID           string
	ProviderType string
}

// AmbiguousTargetError is returned when auto-selection finds more than one
// monitorable database. It carries the candidates so an interactive caller can
// offer a picker; a non-interactive caller just surfaces Error() and the user
// re-runs with --db-instance-id. Kept a distinct type so the command layer can
// errors.As it without string-matching.
type AmbiguousTargetError struct {
	Instances []string // RDS Postgres instance ids
	Clusters  []string // Aurora Postgres cluster ids
}

func (e *AmbiguousTargetError) Error() string {
	return fmt.Sprintf("found multiple databases (RDS %v, Aurora %v); pass --db-instance-id to choose one",
		e.Instances, e.Clusters)
}

// Candidates returns the choices in a stable order (RDS instances first, then
// Aurora clusters), for display in a picker.
func (e *AmbiguousTargetError) Candidates() []TargetChoice {
	cs := make([]TargetChoice, 0, len(e.Instances)+len(e.Clusters))
	for _, id := range e.Instances {
		cs = append(cs, TargetChoice{ID: id, ProviderType: "aws_rds"})
	}
	for _, id := range e.Clusters {
		cs = append(cs, TargetChoice{ID: id, ProviderType: "aws_aurora"})
	}
	return cs
}

// selectSolo picks the single instance-or-cluster from the two id lists, tagging
// its provider kind. Returns an *AmbiguousTargetError on more than one (so an
// interactive caller can prompt) and a plain error on none. Pure, for testability.
func selectSolo(instances, clusters []string) (id, kind string, err error) {
	switch total := len(instances) + len(clusters); {
	case total == 0:
		return "", "", fmt.Errorf("no PostgreSQL RDS instances or Aurora clusters found in this region; pass --db-instance-id")
	case total > 1:
		return "", "", &AmbiguousTargetError{Instances: instances, Clusters: clusters}
	case len(instances) == 1:
		return instances[0], "aws_rds", nil
	default:
		return clusters[0], "aws_aurora", nil
	}
}

// describeInstance reads one RDS instance's details.
func describeInstance(id string) (*rdsInstance, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	out, err := rds.NewFromConfig(cfg).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id),
	})
	if err != nil {
		return nil, fmt.Errorf("could not describe RDS instance %q: %w", id, err)
	}
	if len(out.DBInstances) == 0 {
		return nil, fmt.Errorf("no RDS instance named %q found in this region — check the AWS region "+
			"(AWS_REGION / the active profile) and pass an existing --db-instance-id, then re-run", id)
	}
	inst := instanceFromSDK(out.DBInstances[0])
	if err := checkEngine(inst.Engine, id); err != nil {
		return nil, err
	}
	return inst, nil
}

// instanceFromSDK maps an SDK DBInstance into our plain intermediate.
func instanceFromSDK(in rdstypes.DBInstance) *rdsInstance {
	r := &rdsInstance{
		ID:             aws.ToString(in.DBInstanceIdentifier),
		Engine:         aws.ToString(in.Engine),
		DbiResourceID:  aws.ToString(in.DbiResourceId),
		DBName:         aws.ToString(in.DBName),
		IAMAuthEnabled: aws.ToBool(in.IAMDatabaseAuthenticationEnabled),
	}
	if in.Endpoint != nil {
		r.Host = aws.ToString(in.Endpoint.Address)
		r.Port = int(aws.ToInt32(in.Endpoint.Port))
	}
	if in.DBSubnetGroup != nil {
		for _, sn := range in.DBSubnetGroup.Subnets {
			r.Subnets = append(r.Subnets, aws.ToString(sn.SubnetIdentifier))
		}
	}
	for _, g := range in.VpcSecurityGroups {
		r.SecurityGroups = append(r.SecurityGroups, sgMembership{ID: aws.ToString(g.VpcSecurityGroupId), Status: aws.ToString(g.Status)})
	}
	return r
}

// Complete reports whether every field the Fargate deploy needs is set, so a
// fully-specified target can skip the AWS discovery round-trips entirely.
func (t AwsTarget) Complete() bool {
	return t.InstanceID != "" && t.DbiResourceID != "" && t.Host != "" &&
		len(t.Databases) > 0 && len(t.Subnets) > 0 && t.SecurityGroup != ""
}

// --- collector config for the aws target -----------------------------------
//
// One collector can monitor N databases. The whole configuration — identity,
// endpoints, and one [[component]] per database — is rendered as the collector's
// native TOML and handed to the stack as a single base64 parameter, so the
// CloudFormation template stays static and can be hosted, reviewed, and launched
// from the console. A single-database install is just the N=1 case.
//
// Secrets are never rendered: the config references them as ${VAR}, and the task
// definition injects the real values from Secrets Manager.

// maxConfigParamBytes is CloudFormation's per-parameter value limit. The
// base64-encoded config must fit, so we check it here and say so plainly rather
// than letting CreateStack fail with a generic validation error.
const maxConfigParamBytes = 4096

// awsConfigTOML renders the collector's TOML config for the Fargate target: the
// [dbgorilla] identity block plus one [[component]] per monitored database.
func awsConfigTOML(agentID, tenantID, region string, targets []AwsTarget, eps Endpoints, commandsEnabled bool) (string, error) {
	cfg := Config{
		Dbgorilla: Dbgorilla{
			AgentID:      agentID,
			TenantID:     tenantID,
			Secret:       "${" + SecretEnv + "}",
			OpampBaseURL: eps.OpampBaseURL,
			OtlpBaseURL:  eps.OtlpBaseURL,
			AuthBaseURL:  eps.AuthBaseURL,
		},
		Topology: Topology{Interval: "60s"},
		Commands: Commands{Enabled: commandsEnabled},
	}
	for _, t := range targets {
		cfg.Component = append(cfg.Component, awsComponent(t, region))
	}
	return cfg.Render()
}

// awsComponent maps one discovered database onto a [[component]]. Aurora
// addresses the cluster by cluster_id, standalone RDS by instance_id (the
// collector's provider contract). IAM is the default auth; password auth points
// at the Secrets Manager-backed variable the task definition supplies.
func awsComponent(t AwsTarget, region string) Component {
	port := t.Port
	if port == 0 {
		port = 5432
	}
	provider := Provider{Type: orDefault(t.ProviderType, "aws_rds"), Region: region}
	if provider.Type == "aws_aurora" {
		provider.ClusterID = t.InstanceID
	} else {
		provider.InstanceID = t.InstanceID
	}
	auth := Auth{Method: "iam", User: orDefault(t.User, DefaultDBUser)}
	if t.AuthMethod == "password" {
		auth.Method = "password"
		auth.Password = "${" + AwsDBPasswordEnv + "}"
	}
	// No ca_cert: the collector image trusts the Amazon RDS roots system-wide
	// (0.3.2+), so verification works the same under password auth as under IAM.
	// An operator monitoring a database behind a private CA still sets one.
	connect := Connect{
		Host:      t.Host,
		Port:      port,
		Databases: t.Databases,
		SSLMode:   orDefault(t.SSLMode, "verify-full"),
	}
	return Component{
		Name:     t.Name,
		Engine:   "postgres",
		Commands: t.Commands,
		Provider: provider,
		Auth:     auth,
		Connect:  connect,
	}
}

// AwsStackInput is everything the Fargate stack needs: the minted identity, the
// image, the monitored databases, and the networking discovery resolved.
type AwsStackInput struct {
	AgentID   string
	TenantID  string
	Image     string
	Endpoints Endpoints
	Region    string
	AccountID string
	Targets   []AwsTarget
	// Subnets and SecurityGroup are the task's own networking, which may differ
	// from the databases' (see AwsTarget.SecurityGroup).
	Subnets         []string
	SecurityGroup   string
	AssignPublicIP  string
	CommandsEnabled bool
	ServerSecret    string
	DBPassword      string
}

// AwsStackParams renders the CloudFormation parameter set for the collector
// stack. The monitored databases ride in two of them — the base64 config and
// the matching rds-db:connect grants — which is what keeps the template static
// and publishable.
func AwsStackParams(in AwsStackInput) (map[string]string, error) {
	configTOML, err := awsConfigTOML(in.AgentID, in.TenantID, in.Region, in.Targets, in.Endpoints, in.CommandsEnabled)
	if err != nil {
		return nil, err
	}
	encoded, err := EncodeConfig(configTOML)
	if err != nil {
		return nil, err
	}
	arns := rdsConnectParam(in.Targets, in.Region, in.AccountID)
	return map[string]string{
		configParamKey:     encoded,
		rdsConnectParamKey: strings.Join(arns, ","),
		"ServerSecret":     in.ServerSecret,
		"DbPassword":       in.DBPassword,
		"CollectorImage":   in.Image,
		"Subnets":          strings.Join(in.Subnets, ","),
		"SecurityGroupId":  in.SecurityGroup,
		"AssignPublicIp":   in.AssignPublicIP,
	}, nil
}

// CompactConfig strips whole-line comments and blank lines from a collector
// config. The CloudFormation parameter that carries the config is capped at
// 4096 bytes, and a well-documented config spends most of that on comments the
// collector ignores — so a hand-written config that would not otherwise fit
// does once its prose is dropped.
//
// It only removes lines whose first non-space character is '#', so a '#' inside
// a value is never touched. Multi-line strings are the one place that rule
// breaks (a '#' at the start of a line inside one is content, not a comment),
// so a config containing any is returned unchanged rather than risk corrupting
// it. The result is re-parsed by the caller either way.
func CompactConfig(configTOML string) string {
	if strings.Contains(configTOML, `"""`) || strings.Contains(configTOML, "'''") {
		return configTOML
	}
	var kept []string
	for _, line := range strings.Split(configTOML, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n") + "\n"
}

// EncodeConfig base64-encodes a rendered collector.toml for the stack's
// CollectorConfig parameter, rejecting anything past CloudFormation's parameter
// limit. Single-line, unpadded-safe standard encoding, matching the template's
// AllowedPattern.
func EncodeConfig(configTOML string) (string, error) {
	encoded := base64.StdEncoding.EncodeToString([]byte(configTOML))
	if len(encoded) > maxConfigParamBytes {
		return "", fmt.Errorf(
			"collector config is too large for a CloudFormation parameter: %d bytes encoded, limit is %d. "+
				"Monitor fewer databases per collector, or split them across two installs",
			len(encoded), maxConfigParamBytes)
	}
	return encoded, nil
}

// DecodeConfig reverses EncodeConfig, so an update can read back the config the
// install stored on the stack.
func DecodeConfig(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("stack parameter %s is not valid base64: %w", configParamKey, err)
	}
	return string(raw), nil
}

// rdsConnectParam is rdsConnectARNs with the empty case handled. Every target
// using password auth means the task role needs no IAM DB grant at all, but
// CloudFormation rejects an empty CommaDelimitedList — so pass an ARN that
// matches nothing rather than widening to the template's wildcard default.
// Both the install and the update path must apply this, or an all-password
// update fails stack validation.
func rdsConnectParam(targets []AwsTarget, region, accountID string) []string {
	if arns := rdsConnectARNs(targets, region, accountID); len(arns) > 0 {
		return arns
	}
	return []string{fmt.Sprintf("arn:aws:rds-db:%s:%s:dbuser:none/none", region, accountID)}
}

// rdsConnectARNs returns the rds-db:connect resources the task role needs: one
// ARN per distinct (DbiResourceId, user) pair, so two databases on the same
// instance share a grant — least privilege without redundant statements.
// Password-auth targets don't use IAM DB auth and contribute none.
func rdsConnectARNs(targets []AwsTarget, region, accountID string) []string {
	seen := map[string]bool{}
	arns := []string{}
	for _, t := range targets {
		if t.AuthMethod == "password" {
			continue
		}
		user := orDefault(t.User, DefaultDBUser)
		key := t.DbiResourceID + "/" + user
		if seen[key] {
			continue
		}
		seen[key] = true
		arns = append(arns, fmt.Sprintf("arn:aws:rds-db:%s:%s:dbuser:%s/%s", region, accountID, t.DbiResourceID, user))
	}
	return arns
}

// --- IAM database grant (printed, or run with --run-grant) -----------------

// GrantStatements is the SQL a database admin runs so the collector's IAM user
// can authenticate (rds_iam) and read the database. Shared by the printed
// instructions and the optional automated grant, so the two never drift.
//
// Why each grant:
//   - rds_iam: IAM database authentication.
//   - pg_monitor: the pg_stat_* views behind metrics and query insights.
//   - pg_read_all_data (PG 14+): the topology scraper runs pg_dump, whose
//     ACCESS SHARE table locks require SELECT on every table -- pg_monitor
//     alone leaves topology failing on every scrape (verified live: RDS PG,
//     "permission denied for table ..."). This DOES let the collector read
//     table contents, not just schema; there is no narrower builtin that
//     covers future tables. A policy that forbids it can drop this line at
//     the cost of the schema-topology feature.
func GrantStatements(user string, databases []string) []string {
	u := quoteIdent(user)
	stmts := []string{
		"CREATE USER " + u + " WITH LOGIN;",
		"GRANT rds_iam TO " + u + ";",
		"GRANT pg_monitor TO " + u + ";",
	}
	for _, db := range databases {
		stmts = append(stmts, "GRANT CONNECT ON DATABASE "+quoteIdent(db)+" TO "+u+";")
	}
	// Last on purpose: pg_read_all_data does not exist before PG 14, and
	// RunGrant stops at the first hard error -- everything above still lands.
	stmts = append(stmts, "GRANT pg_read_all_data TO "+u+";")
	return stmts
}

// RunGrant connects to a database as an admin and executes the grant statements.
// It tolerates "already exists" / "already a member" so re-running an install is
// safe. Connectivity from the caller's host to the database is required — a
// private RDS unreachable from here returns a connection error, and the command
// layer falls back to printing the SQL.
func RunGrant(ctx context.Context, dsn string, statements []string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	// The grants are already committed by the time this runs; a close error says
	// nothing the caller can act on.
	defer func() { _ = conn.Close(ctx) }()
	for _, s := range statements {
		if _, err := conn.Exec(ctx, s); err != nil {
			if isBenignGrantErr(err) {
				continue
			}
			return fmt.Errorf("%s -> %w", s, err)
		}
	}
	return nil
}

func isBenignGrantErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already exists") || strings.Contains(m, "already a member")
}

// quoteIdent double-quotes a SQL identifier (user / database name).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// FargateDeploy deploys the collector stack via CloudFormation (aws-sdk-go-v2).
// The template is static — every input, including the monitored databases, is a
// stack parameter — so the same file the CLI ships is the one published for
// console launches. Create-or-update is idempotent, so re-running is safe. Its
// methods live in awscfn.go.
type FargateDeploy struct {
	StackName string
	Params    map[string]string
	DryRun    bool // validate the template without creating/updating anything
	// TemplateURL overrides the published template this deploy uses. Empty means
	// the version-pinned default. Either way it must be reachable — there is no
	// local copy to fall back to.
	TemplateURL string
}

// --- ECS service lifecycle + logs (aws target) -----------------------------

// LogGroupFor is the CloudWatch log group the Fargate stack creates for the
// collector (mirrors the template's LogGroup name).
func LogGroupFor(stackName string) string {
	return "/dbgorilla/collector/" + stackName
}

// Stack parameters the CLI addresses by name.
const (
	// configParamKey carries the whole collector config (base64 TOML), so the
	// monitored databases are a parameter rather than part of the template body.
	configParamKey = "CollectorConfig"
	// rdsConnectParamKey carries the per-database rds-db:connect grants, which
	// CloudFormation expands into the task role's policy.
	rdsConnectParamKey = "RdsConnectResources"
)

// fargateParamKeys lists every CloudFormation parameter of the collector stack.
// UpgradeImage holds all of them except the image at their previous value, so
// an upgrade preserves the monitored databases and their IAM grants.
var fargateParamKeys = []string{
	configParamKey, rdsConnectParamKey, "ServerSecret", "DbPassword",
	"CollectorImage", "Subnets", "SecurityGroupId", "AssignPublicIp",
}

// --- helpers ---------------------------------------------------------------

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
