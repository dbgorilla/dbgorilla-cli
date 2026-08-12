package collector

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// The SDK-to-intermediate mappers are where a field silently going missing
// costs the most: a dropped DbiResourceId or subnet produces a stack that
// deploys cleanly and then cannot reach the database. These pin every field.

func TestInstanceFromSDK_MapsEveryFieldWeDependOn(t *testing.T) {
	in := rdstypes.DBInstance{
		DBInstanceIdentifier:             aws.String("prod-db"),
		Engine:                           aws.String("postgres"),
		DbiResourceId:                    aws.String("db-ABCDEF"),
		DBName:                           aws.String("appdb"),
		IAMDatabaseAuthenticationEnabled: aws.Bool(true),
		Endpoint: &rdstypes.Endpoint{
			Address: aws.String("prod-db.abc.eu-west-1.rds.amazonaws.com"),
			Port:    aws.Int32(5432),
		},
		DBSubnetGroup: &rdstypes.DBSubnetGroup{
			Subnets: []rdstypes.Subnet{
				{SubnetIdentifier: aws.String("subnet-1")},
				{SubnetIdentifier: aws.String("subnet-2")},
			},
		},
		VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{
			{VpcSecurityGroupId: aws.String("sg-old"), Status: aws.String("removing")},
			{VpcSecurityGroupId: aws.String("sg-live"), Status: aws.String("active")},
		},
	}

	got := instanceFromSDK(in)

	if got.ID != "prod-db" || got.Engine != "postgres" {
		t.Errorf("id/engine = %q/%q", got.ID, got.Engine)
	}
	if got.DbiResourceID != "db-ABCDEF" {
		t.Errorf("DbiResourceID = %q — IAM auth policies are written against this", got.DbiResourceID)
	}
	if got.DBName != "appdb" || !got.IAMAuthEnabled {
		t.Errorf("dbname=%q iam=%v", got.DBName, got.IAMAuthEnabled)
	}
	if got.Host != "prod-db.abc.eu-west-1.rds.amazonaws.com" || got.Port != 5432 {
		t.Errorf("endpoint = %s:%d", got.Host, got.Port)
	}
	if len(got.Subnets) != 2 || got.Subnets[0] != "subnet-1" || got.Subnets[1] != "subnet-2" {
		t.Errorf("subnets = %v, want both in order", got.Subnets)
	}
	if len(got.SecurityGroups) != 2 {
		t.Fatalf("security groups = %v, want both memberships preserved", got.SecurityGroups)
	}
	// Filtering by status is the caller's job (firstActiveSG); the mapper keeps
	// everything so an inactive group can still be reported.
	if firstActiveSG(got.SecurityGroups) != "sg-live" {
		t.Errorf("firstActiveSG = %q, want sg-live", firstActiveSG(got.SecurityGroups))
	}
}

// A described instance mid-creation has no Endpoint and no subnet group. The
// mapper must not panic on those nil pointers.
func TestInstanceFromSDK_NilEndpointAndSubnetGroup(t *testing.T) {
	got := instanceFromSDK(rdstypes.DBInstance{
		DBInstanceIdentifier: aws.String("creating-db"),
		Engine:               aws.String("postgres"),
	})
	if got.Host != "" || got.Port != 0 {
		t.Errorf("host/port = %q/%d, want zero values", got.Host, got.Port)
	}
	if got.Subnets != nil {
		t.Errorf("subnets = %v, want nil", got.Subnets)
	}
}

func TestClusterFromSDK_MapsEveryFieldWeDependOn(t *testing.T) {
	got := clusterFromSDK(rdstypes.DBCluster{
		DBClusterIdentifier:              aws.String("aurora-1"),
		Engine:                           aws.String("aurora-postgresql"),
		Endpoint:                         aws.String("aurora-1.cluster-abc.rds.amazonaws.com"),
		Port:                             aws.Int32(5432),
		DbClusterResourceId:              aws.String("cluster-XYZ"),
		DatabaseName:                     aws.String("appdb"),
		IAMDatabaseAuthenticationEnabled: aws.Bool(true),
		DBSubnetGroup:                    aws.String("aurora-subnets"),
		VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{
			{VpcSecurityGroupId: aws.String("sg-live"), Status: aws.String("active")},
		},
	})

	if got.ID != "aurora-1" || got.Engine != "aurora-postgresql" {
		t.Errorf("id/engine = %q/%q", got.ID, got.Engine)
	}
	if got.Host != "aurora-1.cluster-abc.rds.amazonaws.com" || got.Port != 5432 {
		t.Errorf("endpoint = %s:%d", got.Host, got.Port)
	}
	if got.ResourceID != "cluster-XYZ" {
		t.Errorf("ResourceID = %q", got.ResourceID)
	}
	if got.DatabaseName != "appdb" || !got.IAMAuthEnabled {
		t.Errorf("dbname=%q iam=%v", got.DatabaseName, got.IAMAuthEnabled)
	}
	// A cluster names its subnet group; the subnet ids need a second lookup.
	if got.SubnetGroup != "aurora-subnets" {
		t.Errorf("SubnetGroup = %q", got.SubnetGroup)
	}
	if len(got.SecurityGroups) != 1 || got.SecurityGroups[0].ID != "sg-live" {
		t.Errorf("security groups = %v", got.SecurityGroups)
	}
}

func TestClusterFromSDK_EmptyClusterIsAllZeroValues(t *testing.T) {
	got := clusterFromSDK(rdstypes.DBCluster{})
	if got.ID != "" || got.Port != 0 || got.SecurityGroups != nil {
		t.Errorf("got %+v, want zero values throughout", got)
	}
}

func TestFirstActiveSG_NoneActive(t *testing.T) {
	if id := firstActiveSG([]sgMembership{
		{ID: "sg-a", Status: "adding"},
		{ID: "sg-b", Status: "removing"},
	}); id != "" {
		t.Errorf("firstActiveSG = %q, want empty when nothing is active", id)
	}
	if id := firstActiveSG(nil); id != "" {
		t.Errorf("firstActiveSG(nil) = %q, want empty", id)
	}
}

// The ambiguity error is what a user sees when auto-selection refuses to guess.
// It has to name the candidates and the flag that resolves it.
func TestAmbiguousTargetError_Message(t *testing.T) {
	err := &AmbiguousTargetError{
		Instances: []string{"rds-a", "rds-b"},
		Clusters:  []string{"aurora-c"},
	}
	msg := err.Error()
	for _, want := range []string{"rds-a", "rds-b", "aurora-c", "--db-instance-id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should name %q, got: %s", want, msg)
		}
	}
}

func TestLogGroupFor(t *testing.T) {
	// Must match the log group the CloudFormation template creates, or
	// `dbg collector logs` tails nothing on a healthy stack.
	if got := LogGroupFor("dbg-collector"); got != "/dbgorilla/collector/dbg-collector" {
		t.Errorf("LogGroupFor = %q", got)
	}
}

// Re-running an install must be safe, so the grant runner ignores the errors a
// second run produces and nothing else.
func TestIsBenignGrantErr(t *testing.T) {
	benign := []string{
		`ERROR: role "dbgorilla_ro" already exists (SQLSTATE 42710)`,
		`ERROR: role "dbgorilla_ro" is already a member of role "pg_read_all_data"`,
		`ALREADY EXISTS`, // case-insensitive
	}
	for _, m := range benign {
		if !isBenignGrantErr(errors.New(m)) {
			t.Errorf("should tolerate on re-run: %s", m)
		}
	}
	fatal := []string{
		`ERROR: permission denied for schema public (SQLSTATE 42501)`,
		`ERROR: syntax error at or near "GRANT"`,
		`connection refused`,
	}
	for _, m := range fatal {
		if isBenignGrantErr(errors.New(m)) {
			t.Errorf("must NOT swallow: %s", m)
		}
	}
}

func TestQuoteIdent_EscapesEmbeddedQuotes(t *testing.T) {
	// A database named with a quote must not be able to break out of the
	// identifier and append SQL.
	if got := quoteIdent(`ro"; DROP DATABASE x; --`); got != `"ro""; DROP DATABASE x; --"` {
		t.Errorf("quoteIdent = %s", got)
	}
	if got := quoteIdent("plain"); got != `"plain"` {
		t.Errorf("quoteIdent = %s", got)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault("", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := orDefault("set", "fallback"); got != "set" {
		t.Errorf("got %q", got)
	}
}

func TestActiveSGs_OrderPreservedAndInactiveDropped(t *testing.T) {
	got := activeSGs([]sgMembership{
		{ID: "sg-1", Status: "active"},
		{ID: "sg-2", Status: "adding"},
		{ID: "sg-3", Status: "active"},
	})
	if len(got) != 2 || got[0] != "sg-1" || got[1] != "sg-3" {
		t.Errorf("activeSGs = %v, want [sg-1 sg-3]", got)
	}
	if activeSGs(nil) != nil {
		t.Error("activeSGs(nil) should be nil")
	}
}

// DecodeConfig is what the dry run uses to show the TOML it would deploy. A
// blob that is not valid base64 must report that, not print garbage.
func TestDecodeConfig_RejectsNonBase64(t *testing.T) {
	if _, err := DecodeConfig("not base64 !!!"); err == nil {
		t.Fatal("expected an error for a non-base64 blob")
	}
}
