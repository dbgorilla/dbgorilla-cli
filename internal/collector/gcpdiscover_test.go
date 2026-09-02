package collector

import (
	"errors"
	"strings"
	"testing"
)

// Discovery decides what the deployed collector connects to. Every branch here
// was reachable only against a live project before.

const (
	sqlListPath  = "/v1/projects/p/instances"
	adbClusters  = "/v1/projects/p/locations/-/clusters"
	adbInstances = "/v1/projects/p/locations/us-east1/clusters/orders/instances"
	ordersJSON   = `{"clusters":[{"name":"projects/p/locations/us-east1/clusters/orders"}]}`
	primaryJSON  = `{"instances":[
		{"name":"projects/p/locations/us-east1/clusters/orders/instances/orders-pool","instanceType":"READ_POOL","ipAddress":"10.1.2.9"},
		{"name":"projects/p/locations/us-east1/clusters/orders/instances/orders-primary","instanceType":"PRIMARY","ipAddress":"10.1.2.3"}
	]}`
)

func TestDiscoverGcpTarget_SoloCloudSQL(t *testing.T) {
	f := newGCPFake(t).
		on("GET", sqlListPath, 200, sqlInstancesJSON(sqlInstanceJSON("prod-pg", "POSTGRES_16", ""))).
		on("GET", adbClusters, 200, `{}`).
		on("GET", sqlListPath+"/prod-pg", 200, sqlInstanceJSON("prod-pg", "POSTGRES_16", ""))
	stubGCP(t, f)

	got, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p", Databases: []string{"app"}})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got.ProviderType != "cloud_sql" || got.InstanceID != "prod-pg" || got.Region != "us-central1" {
		t.Fatalf("identity: %+v", got)
	}
	if got.Host != "abc.def.us-central1.sql-psa.goog." || !got.IamEnabled || got.Network != "projects/p/global/networks/default" {
		t.Fatalf("facts: %+v", got)
	}
	// Explicit seed fields survive discovery.
	if len(got.Databases) != 1 || got.Databases[0] != "app" {
		t.Errorf("seed databases lost: %v", got.Databases)
	}
}

// The listing already saw the cluster's location; discovery must reuse it
// rather than list every cluster a second time to recover it.
func TestDiscoverGcpTarget_SoloAlloyDBReusesTheListedLocation(t *testing.T) {
	f := newGCPFake(t).
		on("GET", sqlListPath, 200, sqlInstancesJSON()).
		on("GET", adbClusters, 200, ordersJSON).
		on("GET", adbInstances, 200, primaryJSON)
	stubGCP(t, f)

	got, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got.ProviderType != "alloydb" || got.ClusterID != "orders" || got.InstanceID != "orders-primary" {
		t.Fatalf("identity: %+v", got)
	}
	if got.Region != "us-east1" || got.Host != "10.1.2.3" || !got.IamEnabled || got.Engine != "postgres" {
		t.Fatalf("facts: %+v", got)
	}
	if n := f.called("GET", adbClusters); n != 1 {
		t.Errorf("clusters listed %d times; the location from the first listing should be reused", n)
	}
}

func TestDiscoverGcpTarget_ExplicitAlloyDBIdLooksUpTheRegionOnce(t *testing.T) {
	f := newGCPFake(t).
		on("GET", adbClusters, 200, ordersJSON).
		on("GET", adbInstances, 200, primaryJSON)
	stubGCP(t, f)

	t.Run("cluster/instance", func(t *testing.T) {
		got, err := DiscoverGcpTarget("orders/orders-primary", "", GcpTarget{Project: "p"})
		if err != nil || got.Region != "us-east1" || got.InstanceID != "orders-primary" {
			t.Fatalf("got %+v, %v", got, err)
		}
		if f.called("GET", sqlListPath) != 0 {
			t.Error("an alloydb id must not touch the Cloud SQL API")
		}
	})
	t.Run("naming a read pool is refused", func(t *testing.T) {
		_, err := DiscoverGcpTarget("orders/orders-pool", "", GcpTarget{Project: "p"})
		if err == nil || !strings.Contains(err.Error(), "READ_POOL") {
			t.Fatalf("err = %v, want the read-pool refusal", err)
		}
	})
	t.Run("an unknown cluster names the fix", func(t *testing.T) {
		_, err := DiscoverGcpTarget("nope", "alloydb", GcpTarget{Project: "p"})
		if err == nil || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("err = %v, want the project hint", err)
		}
	})
}

func TestDiscoverGcpTarget_AmbiguityIsTypedAndLabelled(t *testing.T) {
	stubGCP(t, newGCPFake(t).
		on("GET", sqlListPath, 200, sqlInstancesJSON(
			sqlInstanceJSON("b-pg", "POSTGRES_16", ""), sqlInstanceJSON("a-my", "MYSQL_8_0", ""))).
		on("GET", adbClusters, 200, ordersJSON).
		on("GET", adbInstances, 200, primaryJSON))

	_, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p"})
	var amb *AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want *AmbiguousTargetError", err)
	}
	cands := amb.Candidates()
	if len(cands) != 3 || cands[0].ID != "a-my" || cands[1].ID != "b-pg" || cands[2].ProviderType != "alloydb" {
		t.Fatalf("candidates: %+v (want sorted Cloud SQL, then AlloyDB)", cands)
	}
	for _, want := range []string{"a-my (Cloud SQL)", "orders/orders-primary (AlloyDB)", "--db-instance-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should carry %q, got: %v", want, err)
		}
	}
}

func TestDiscoverGcpTarget_NoneFoundNamesTheFlag(t *testing.T) {
	stubGCP(t, newGCPFake(t).
		on("GET", sqlListPath, 200, sqlInstancesJSON()).
		on("GET", adbClusters, 200, `{}`))
	_, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p"})
	var amb *AmbiguousTargetError
	if err == nil || errors.As(err, &amb) || !strings.Contains(err.Error(), "--db-instance-id") {
		t.Fatalf("err = %v, want a plain actionable error", err)
	}
}

// A provider hint narrows the listing, so a Cloud SQL-only install needs no
// AlloyDB permission (and vice versa).
func TestDiscoverGcpTarget_HintSkipsTheOtherAPI(t *testing.T) {
	f := newGCPFake(t).
		on("GET", sqlListPath, 200, sqlInstancesJSON(sqlInstanceJSON("prod-pg", "POSTGRES_16", ""))).
		on("GET", sqlListPath+"/prod-pg", 200, sqlInstanceJSON("prod-pg", "POSTGRES_16", ""))
	stubGCP(t, f)
	if _, err := DiscoverGcpTarget("", "cloud_sql", GcpTarget{Project: "p"}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if f.called("GET", adbClusters) != 0 {
		t.Error("a cloud_sql hint must not list AlloyDB clusters")
	}
}

func TestDiscoverGcpTarget_PaginatesTheInstanceListing(t *testing.T) {
	f := newGCPFake(t).
		onSeq("GET", sqlListPath,
			gcpFakeResp{200, sqlInstancesPageJSON("p2", sqlInstanceJSON("a-pg", "POSTGRES_16", ""))},
			gcpFakeResp{200, sqlInstancesJSON(sqlInstanceJSON("b-pg", "POSTGRES_16", ""))}).
		on("GET", adbClusters, 200, `{}`)
	stubGCP(t, f)
	_, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p"})
	var amb *AmbiguousTargetError
	if !errors.As(err, &amb) || len(amb.Candidates()) != 2 {
		t.Fatalf("both pages must count: err = %v", err)
	}
	if c := f.lastCall("GET " + sqlListPath + "?"); !strings.Contains(c, "pageToken=p2") {
		t.Errorf("the second page must be requested by token, got %s", c)
	}
}

func TestDiscoverGcpTarget_RefusesWhatTheCollectorCannotMonitor(t *testing.T) {
	stubGCP(t, newGCPFake(t).
		on("GET", sqlListPath+"/replica", 200, sqlInstanceJSON("replica", "POSTGRES_16", "prod-pg")).
		on("GET", sqlListPath+"/mssql", 200, sqlInstanceJSON("mssql", "SQLSERVER_2019_STANDARD", "")).
		on("GET", sqlListPath+"/gone", 404, gcpNotFoundJSON))

	cases := map[string]string{
		"replica": "read replica",
		"mssql":   "does not support",
		"gone":    "--project",
	}
	for id, want := range cases {
		_, err := DiscoverGcpTarget(id, "", GcpTarget{Project: "p"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", id, err, want)
		}
	}
}

func TestDiscoverGcpTarget_NeedsAProject(t *testing.T) {
	stubGCP(t, newGCPFake(t))
	_, err := DiscoverGcpTarget("", "", GcpTarget{})
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("err = %v, want the --project hint", err)
	}
}

func TestDiscoverGcpTarget_ListFailureNamesTheAPI(t *testing.T) {
	stubGCP(t, newGCPFake(t).on("GET", sqlListPath, 403,
		`{"error":{"code":403,"message":"Cloud SQL Admin API has not been used in project p","status":"PERMISSION_DENIED"}}`))
	_, err := DiscoverGcpTarget("", "", GcpTarget{Project: "p"})
	if err == nil || !strings.Contains(err.Error(), "Cloud SQL Admin API") || !strings.Contains(err.Error(), "cloudsql.viewer") {
		t.Fatalf("err = %v, want the API's message plus the role hint", err)
	}
}

func TestGcpIdentity(t *testing.T) {
	t.Run("email when the token carries one", func(t *testing.T) {
		f := newGCPFake(t).on("POST", "/tokeninfo", 200, `{"email":"dev@example.com"}`)
		stubGCP(t, f)
		got, err := GcpIdentity()
		if err != nil || got != "dev@example.com" {
			t.Fatalf("got (%q, %v)", got, err)
		}
		// The token rides the form body, never the URL.
		if c := f.lastCall("POST /tokeninfo"); strings.Contains(c, "test-token") {
			t.Errorf("token leaked into the URL: %s", c)
		}
		if !strings.Contains(f.lastBody("POST", "/tokeninfo"), "access_token=test-token") {
			t.Error("token should be posted in the form body")
		}
	})
	t.Run("a rejected token is actionable", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).on("POST", "/tokeninfo", 400, `{"error":"invalid_token"}`))
		if err := GcpAvailable(); err == nil || !strings.Contains(err.Error(), "gcloud auth application-default login") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no email still authenticates", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).on("POST", "/tokeninfo", 200, `{}`))
		got, err := GcpIdentity()
		if err != nil || !strings.Contains(got, "authenticated") {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
}

func TestGcpProject(t *testing.T) {
	stubGCP(t, newGCPFake(t))
	if got, err := GcpProject(); err != nil || got != "test-project" {
		t.Fatalf("got (%q, %v)", got, err)
	}
}
