package collector

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Discovery is what turns "--db-instance-id prod-db" (or nothing at all) into
// the host, subnets and resource id the Fargate stack is built from. Every
// branch here previously ran for the first time against a customer's account.

func TestDescribeInstance_MapsAndValidatesEngine(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBInstances",
		describeInstancesXML(instanceXML("prod-db", "postgres"))))

	inst, err := describeInstance("prod-db")
	if err != nil {
		t.Fatalf("describeInstance: %v", err)
	}
	if inst.ID != "prod-db" || inst.Host != "prod-db.abc.us-east-1.rds.amazonaws.com" || inst.Port != 5432 {
		t.Errorf("got %+v", inst)
	}
	if inst.DbiResourceID != "db-RES-prod-db" {
		t.Errorf("DbiResourceID = %q", inst.DbiResourceID)
	}
	if len(inst.Subnets) != 2 {
		t.Errorf("subnets = %v, want both", inst.Subnets)
	}
}

func TestDescribeInstance_UnsupportedEngineIsRefusedNotDeployed(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBInstances",
		describeInstancesXML(instanceXML("mysql-db", "mysql"))))

	_, err := describeInstance("mysql-db")
	if !errors.Is(err, ErrUnsupportedEngine) {
		t.Fatalf("err = %v, want ErrUnsupportedEngine", err)
	}
}

func TestDescribeInstance_NotFoundNamesTheRegionProblem(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBInstances", describeInstancesXML()))

	_, err := describeInstance("missing-db")
	if err == nil {
		t.Fatal("expected an error for an empty result set")
	}
	// The most common cause is the wrong region, not a wrong name.
	if !strings.Contains(err.Error(), "region") || !strings.Contains(err.Error(), "missing-db") {
		t.Errorf("error should name the database and point at the region, got: %v", err)
	}
}

func TestDescribeInstance_APIErrorIsWrapped(t *testing.T) {
	stubAWS(t, newAWSFake(t).fail("DescribeDBInstances", http.StatusForbidden,
		awsErrorXML("AccessDenied", "not authorized to perform rds:DescribeDBInstances")))

	_, err := describeInstance("prod-db")
	if err == nil {
		t.Fatal("expected the API error to surface")
	}
	if !strings.Contains(err.Error(), "prod-db") {
		t.Errorf("wrapped error should name the database, got: %v", err)
	}
}

func TestDescribeInstance_CredentialFailureSurfaces(t *testing.T) {
	stubAWSConfigError(t, errors.New("no valid providers in chain"))
	if _, err := describeInstance("prod-db"); err == nil {
		t.Fatal("a credential failure must not look like a missing database")
	}
}

func TestDescribeCluster_MapsAndResolvesSubnetGroup(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-1", "aurora-postgresql"))).
		on("DescribeDBSubnetGroups", subnetGroupsXML("subnet-x", "subnet-y")))

	target, err := discoverCluster("aurora-1", AwsTarget{})
	if err != nil {
		t.Fatalf("discoverCluster: %v", err)
	}
	if target.Host != "aurora-1.cluster-abc.us-east-1.rds.amazonaws.com" {
		t.Errorf("host = %q", target.Host)
	}
	if target.DbiResourceID != "cluster-RES-aurora-1" {
		t.Errorf("resource id = %q", target.DbiResourceID)
	}
	// A cluster's subnets come from the named group, not the cluster record.
	if len(target.Subnets) != 2 || target.Subnets[0] != "subnet-x" {
		t.Errorf("subnets = %v, want the subnet group's", target.Subnets)
	}
	if target.SecurityGroup != "sg-live" {
		t.Errorf("security group = %q", target.SecurityGroup)
	}
}

func TestDescribeCluster_NotFound(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBClusters", describeClustersXML()))
	if _, err := describeCluster("nope"); err == nil {
		t.Fatal("expected an error for an empty result set")
	}
}

func TestDescribeCluster_WrongEngine(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBClusters",
		describeClustersXML(clusterXML("aurora-my", "aurora-mysql"))))
	if _, err := describeCluster("aurora-my"); !errors.Is(err, ErrUnsupportedEngine) {
		t.Fatalf("err = %v, want ErrUnsupportedEngine", err)
	}
}

func TestSubnetGroupSubnets_ErrorAndEmpty(t *testing.T) {
	t.Run("api error is wrapped with the group name", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeDBSubnetGroups", http.StatusBadRequest,
			awsErrorXML("DBSubnetGroupNotFoundFault", "not found")))
		_, err := subnetGroupSubnets("missing-group")
		if err == nil || !strings.Contains(err.Error(), "missing-group") {
			t.Fatalf("err = %v, want the group named", err)
		}
	})

	t.Run("no groups returns no subnets without erroring", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeDBSubnetGroups",
			`<DescribeDBSubnetGroupsResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
			   <DescribeDBSubnetGroupsResult><DBSubnetGroups></DBSubnetGroups></DescribeDBSubnetGroupsResult>
			 </DescribeDBSubnetGroupsResponse>`))
		subnets, err := subnetGroupSubnets("empty-group")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if subnets != nil {
			t.Errorf("subnets = %v, want nil", subnets)
		}
	})

	t.Run("credential failure surfaces", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("expired SSO session"))
		if _, err := subnetGroupSubnets("g"); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

// --- auto-selection (no --db-instance-id) ---------------------------------

func TestSoloTarget_SingleInstance(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML(instanceXML("only-db", "postgres"))).
		on("DescribeDBClusters", describeClustersXML()))

	id, kind, err := soloTarget()
	if err != nil {
		t.Fatalf("soloTarget: %v", err)
	}
	if id != "only-db" || kind != "aws_rds" {
		t.Errorf("got (%q,%q), want (only-db, aws_rds)", id, kind)
	}
}

func TestSoloTarget_SingleAuroraCluster(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML()).
		on("DescribeDBClusters", describeClustersXML(clusterXML("only-aurora", "aurora-postgresql"))))

	id, kind, err := soloTarget()
	if err != nil {
		t.Fatalf("soloTarget: %v", err)
	}
	if id != "only-aurora" || kind != "aws_aurora" {
		t.Errorf("got (%q,%q), want (only-aurora, aws_aurora)", id, kind)
	}
}

// Non-Postgres databases in the account must not count towards the "exactly
// one" decision, or an account with one Postgres and one MySQL database
// becomes falsely ambiguous.
func TestSoloTarget_IgnoresNonPostgresEngines(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML(
			instanceXML("mysql-db", "mysql"),
			instanceXML("pg-db", "postgres"),
			instanceXML("oracle-db", "oracle-se2"),
		)).
		on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-my", "aurora-mysql"))))

	id, kind, err := soloTarget()
	if err != nil {
		t.Fatalf("soloTarget: %v", err)
	}
	if id != "pg-db" || kind != "aws_rds" {
		t.Errorf("got (%q,%q), want the sole Postgres instance", id, kind)
	}
}

func TestSoloTarget_AmbiguousCarriesCandidates(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML(
			instanceXML("pg-a", "postgres"), instanceXML("pg-b", "postgres"))).
		on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-c", "aurora-postgresql"))))

	_, _, err := soloTarget()
	var amb *AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want *AmbiguousTargetError", err)
	}
	if got := amb.Candidates(); len(got) != 3 || got[0].ID != "pg-a" || got[2].ProviderType != "aws_aurora" {
		t.Errorf("candidates = %+v, want RDS first then Aurora", got)
	}
}

func TestSoloTarget_NoneFound(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML()).
		on("DescribeDBClusters", describeClustersXML()))

	if _, _, err := soloTarget(); err == nil || !strings.Contains(err.Error(), "--db-instance-id") {
		t.Fatalf("err = %v, want a message offering --db-instance-id", err)
	}
}

func TestSoloTarget_ListErrorsAreDistinguishable(t *testing.T) {
	t.Run("instance list fails", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeDBInstances", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		_, _, err := soloTarget()
		if err == nil || !strings.Contains(err.Error(), "AWS permissions") {
			t.Fatalf("err = %v, want the permissions hint", err)
		}
	})

	t.Run("cluster list fails", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).
			on("DescribeDBInstances", describeInstancesXML()).
			fail("DescribeDBClusters", http.StatusForbidden, awsErrorXML("AccessDenied", "denied")))
		_, _, err := soloTarget()
		if err == nil || !strings.Contains(err.Error(), "Aurora") {
			t.Fatalf("err = %v, want the Aurora list named", err)
		}
	})

	t.Run("credentials fail", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if _, _, err := soloTarget(); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

// --- DiscoverAwsTarget: the branch matrix ---------------------------------

func TestDiscoverAwsTarget_ProviderHintSkipsTheGuess(t *testing.T) {
	t.Run("aws_rds goes straight to the instance lookup", func(t *testing.T) {
		f := newAWSFake(t).on("DescribeDBInstances",
			describeInstancesXML(instanceXML("prod-db", "postgres")))
		stubAWS(t, f)

		got, err := DiscoverAwsTarget("prod-db", "aws_rds", AwsTarget{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.InstanceID != "prod-db" {
			t.Errorf("instance id = %q", got.InstanceID)
		}
		for _, call := range f.calls {
			if call == "DescribeDBClusters" {
				t.Error("an explicit aws_rds hint should not also query clusters")
			}
		}
	})

	t.Run("aws_aurora goes straight to the cluster lookup", func(t *testing.T) {
		f := newAWSFake(t).
			on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-1", "aurora-postgresql"))).
			on("DescribeDBSubnetGroups", subnetGroupsXML("subnet-x"))
		stubAWS(t, f)

		got, err := DiscoverAwsTarget("aurora-1", "aws_aurora", AwsTarget{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.ProviderType != "aws_aurora" {
			t.Errorf("provider type = %q", got.ProviderType)
		}
	})
}

// With no hint the lookup tries an instance, then a cluster. The interesting
// case is the fallback actually working.
func TestDiscoverAwsTarget_NoHintFallsBackToCluster(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML()). // not an instance
		on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-1", "aurora-postgresql"))).
		on("DescribeDBSubnetGroups", subnetGroupsXML("subnet-x")))

	got, err := DiscoverAwsTarget("aurora-1", "", AwsTarget{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.InstanceID != "aurora-1" || got.ProviderType != "aws_aurora" {
		t.Errorf("got %+v, want the Aurora cluster", got)
	}
}

// A database that exists but runs the wrong engine is a definite answer. It
// must be reported as such rather than falling through to "no such database",
// which would send the user hunting for a name that is perfectly correct.
func TestDiscoverAwsTarget_WrongEngineBeatsNotFound(t *testing.T) {
	t.Run("instance side", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeDBInstances",
			describeInstancesXML(instanceXML("mysql-db", "mysql"))))
		_, err := DiscoverAwsTarget("mysql-db", "", AwsTarget{})
		if !errors.Is(err, ErrUnsupportedEngine) {
			t.Fatalf("err = %v, want ErrUnsupportedEngine", err)
		}
	})

	t.Run("cluster side", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).
			on("DescribeDBInstances", describeInstancesXML()).
			on("DescribeDBClusters", describeClustersXML(clusterXML("aurora-my", "aurora-mysql"))))
		_, err := DiscoverAwsTarget("aurora-my", "", AwsTarget{})
		if !errors.Is(err, ErrUnsupportedEngine) {
			t.Fatalf("err = %v, want ErrUnsupportedEngine", err)
		}
	})
}

func TestDiscoverAwsTarget_NeitherInstanceNorCluster(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML()).
		on("DescribeDBClusters", describeClustersXML()))

	_, err := DiscoverAwsTarget("ghost", "", AwsTarget{})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "region") {
		t.Errorf("error should name the id and the region, got: %v", err)
	}
}

// With no id at all, discovery auto-selects — and the selection has to feed
// straight into the matching lookup.
func TestDiscoverAwsTarget_EmptyIDAutoSelects(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML(instanceXML("only-db", "postgres"))).
		on("DescribeDBClusters", describeClustersXML()))

	got, err := DiscoverAwsTarget("", "", AwsTarget{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.InstanceID != "only-db" || got.ProviderType != "aws_rds" {
		t.Errorf("got %+v, want the auto-selected instance", got)
	}
}

func TestDiscoverAwsTarget_AutoSelectFailureIsReturnedUnchanged(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeDBInstances", describeInstancesXML()).
		on("DescribeDBClusters", describeClustersXML()))

	into := AwsTarget{Name: "preserved"}
	got, err := DiscoverAwsTarget("", "", into)
	if err == nil {
		t.Fatal("expected the auto-select error")
	}
	if got.Name != "preserved" {
		t.Errorf("the caller's target should come back untouched, got %+v", got)
	}
}

// Explicitly-set fields survive discovery: a user who passed --db-name must not
// have it overwritten by whatever RDS reports.
func TestDiscoverAwsTarget_ExplicitFieldsWin(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeDBInstances",
		describeInstancesXML(instanceXML("prod-db", "postgres"))))

	got, err := DiscoverAwsTarget("prod-db", "aws_rds", AwsTarget{
		Databases: []string{"chosen"},
		Host:      "pinned.example.com",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Host != "pinned.example.com" {
		t.Errorf("host = %q, want the explicit value preserved", got.Host)
	}
	if len(got.Databases) != 1 || got.Databases[0] != "chosen" {
		t.Errorf("databases = %v, want the explicit value preserved", got.Databases)
	}
}
