package collector

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// --- solo-target selection ---------------------------------------------------

// Cloud SQL instances first, then AlloyDB clusters, each tagged with its
// provider type.
func TestListGcpCandidates_OrderAndKinds(t *testing.T) {
	stubGCP(t, newGCPFake(t).
		on("GET", "/v1/projects/p/instances", 200, sqlInstancesJSON(
			sqlInstanceJSON("zeta-pg", "POSTGRES_16", ""),
			sqlInstanceJSON("alpha-my", "MYSQL_8_0", ""),
			sqlInstanceJSON("replica", "POSTGRES_16", "zeta-pg"), // replicas are never targets
			sqlInstanceJSON("legacy-sqlserver", "SQLSERVER_2019_STANDARD", ""))).
		on("GET", "/v1/projects/p/locations/-/clusters/-/instances", 200,
			`{"instances":[
				{"name":"projects/p/locations/us-east1/clusters/orders/instances/orders-pool","instanceType":"READ_POOL","ipAddress":"10.1.2.9"},
				{"name":"projects/p/locations/us-east1/clusters/orders/instances/orders-primary","instanceType":"PRIMARY","ipAddress":"10.1.2.3"}
			]}`))
	cfg, _ := loadGCPConfig(context.Background())
	got, err := listGcpCandidates(context.Background(), cfg, "p", "")
	if err != nil {
		t.Fatalf("listGcpCandidates: %v", err)
	}
	want := []TargetChoice{
		{ID: "alpha-my", ProviderType: "cloud_sql"},
		{ID: "zeta-pg", ProviderType: "cloud_sql"},
		{ID: "orders/orders-primary", ProviderType: "alloydb"},
	}
	if !reflect.DeepEqual(got.choices, want) {
		t.Fatalf("choices:\n got  %+v\n want %+v", got.choices, want)
	}
	if got.locations["orders/orders-primary"] != "us-east1" {
		t.Fatalf("locations: %v", got.locations)
	}
}

// --- Cloud SQL field mapping -------------------------------------------------

func TestMergeCloudSQLInstance_FieldByField(t *testing.T) {
	info := &sqlInstanceInfo{
		Name:            "prod-pg",
		Region:          "us-central1",
		DatabaseVersion: "POSTGRES_16",
	}
	info.DNSNames = []struct {
		Name           string `json:"name"`
		ConnectionType string `json:"connectionType"`
	}{
		// PSC listed first: the PSA mapping must win by connectionType.
		{Name: "abc.def.us-central1.sql-psc.goog.", ConnectionType: "PRIVATE_SERVICE_CONNECT"},
		{Name: "abc.def.us-central1.sql-psa.goog.", ConnectionType: "PRIVATE_SERVICES_ACCESS"},
	}
	info.IPAddresses = []struct {
		Type      string `json:"type"`
		IPAddress string `json:"ipAddress"`
	}{{Type: "PRIVATE", IPAddress: "10.0.0.4"}}
	info.Settings.IPConfiguration.ServerCaMode = "GOOGLE_MANAGED_CAS_CA"
	info.Settings.IPConfiguration.PrivateNetwork = "projects/p/global/networks/default"
	info.Settings.DatabaseFlags = []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{{Name: "cloudsql.iam_authentication", Value: "on"}}

	got := mergeCloudSQLInstance(GcpTarget{Project: "p"}, info)
	if got.ProviderType != "cloud_sql" || got.InstanceID != "prod-pg" || got.Region != "us-central1" {
		t.Fatalf("identity: %+v", got)
	}
	if got.Engine != "postgres" || got.Port != 5432 {
		t.Fatalf("engine: %+v", got)
	}
	// The DNS name is what the certificate attests, trailing dot included.
	if got.Host != "abc.def.us-central1.sql-psa.goog." {
		t.Fatalf("host must be the verbatim DNS name, got %q", got.Host)
	}
	if got.ServerCaMode != "GOOGLE_MANAGED_CAS_CA" || !got.IamEnabled {
		t.Fatalf("tls/iam facts: %+v", got)
	}
	if got.Network != "projects/p/global/networks/default" {
		t.Fatalf("network: %q", got.Network)
	}
}

func TestMergeCloudSQLInstance_FallsBackToPrivateIPAndDefaultsTheCA(t *testing.T) {
	info := &sqlInstanceInfo{Name: "legacy-pg", DatabaseVersion: "POSTGRES_14"}
	info.IPAddresses = []struct {
		Type      string `json:"type"`
		IPAddress string `json:"ipAddress"`
	}{
		{Type: "PRIMARY", IPAddress: "34.1.2.3"},
		{Type: "PRIVATE", IPAddress: "10.0.0.9"},
	}
	got := mergeCloudSQLInstance(GcpTarget{}, info)
	if got.Host != "10.0.0.9" {
		t.Fatalf("private IP must beat the public one, got %q", got.Host)
	}
	if got.ServerCaMode != "GOOGLE_MANAGED_INTERNAL_CA" {
		t.Fatalf("absent CA mode must read as the internal CA, got %q", got.ServerCaMode)
	}
	if got.IamEnabled {
		t.Fatal("no flag means IAM auth is off for Cloud SQL")
	}
}

func TestMergeCloudSQLInstance_MySQLGetsItsEngineAndPort(t *testing.T) {
	got := mergeCloudSQLInstance(GcpTarget{}, &sqlInstanceInfo{
		Name: "prod-my", DatabaseVersion: "MYSQL_8_0",
	})
	if got.Engine != "mysql" || got.Port != 3306 {
		t.Fatalf("mysql mapping: %+v", got)
	}
}

// --- config rendering --------------------------------------------------------

func TestGcpComponent_CloudSQLIamAuth(t *testing.T) {
	c := gcpComponent(GcpTarget{
		ProviderType: "cloud_sql",
		Project:      "acme-prod",
		Region:       "us-central1",
		InstanceID:   "orders-pg",
		Engine:       "postgres",
		Host:         "abc.us-central1.sql-psa.goog.",
		Port:         5432,
		ServerCaMode: "GOOGLE_MANAGED_CAS_CA",
		AuthMethod:   "gcp_iam",
		User:         "collector@acme-prod.iam.gserviceaccount.com",
	})
	if c.Provider.Type != "cloud_sql" || c.Provider.Project != "acme-prod" ||
		c.Provider.Instance != "orders-pg" || c.Provider.Cluster != "" {
		t.Fatalf("provider block: %+v", c.Provider)
	}
	if c.Auth.Method != "gcp_iam" || c.Auth.Password != "" {
		t.Fatalf("iam auth must carry no password reference: %+v", c.Auth)
	}
	if c.Connect.SSLMode != "verify-full" {
		t.Fatalf("token auth pins verify-full, got %q", c.Connect.SSLMode)
	}
}

func TestGcpComponent_PasswordOnInternalCAConnectsAtRequire(t *testing.T) {
	// The default per-instance CA attests no hostname and the collector no
	// longer fetches it at discovery — an unpinned verify-ca is refused on
	// every provider, so `require` is the mode this hosting gets.
	c := gcpComponent(GcpTarget{
		ProviderType: "cloud_sql",
		Project:      "p",
		Region:       "r",
		InstanceID:   "i",
		Engine:       "postgres",
		Host:         "10.0.0.4",
		Port:         5432,
		ServerCaMode: "GOOGLE_MANAGED_INTERNAL_CA",
		AuthMethod:   "password",
		User:         "dbg_readonly",
	})
	if c.Connect.SSLMode != "require" {
		t.Fatalf("expected require on the internal CA, got %q", c.Connect.SSLMode)
	}
	if c.Auth.Password != "${"+CloudDBPasswordEnv+"}" {
		t.Fatalf("password must be an env reference, got %q", c.Auth.Password)
	}
}

func TestGcpComponent_AlloyDBNamesClusterAndPrimary(t *testing.T) {
	c := gcpComponent(GcpTarget{
		ProviderType: "alloydb",
		Project:      "acme-prod",
		Region:       "us-central1",
		ClusterID:    "orders",
		InstanceID:   "orders-primary",
		Engine:       "postgres",
		Host:         "10.1.2.3",
		Port:         5432,
		AuthMethod:   "gcp_iam",
		User:         "collector@acme-prod.iam",
	})
	if c.Name != "orders" {
		t.Fatalf("the cluster names the component, got %q", c.Name)
	}
	if c.Provider.Cluster != "orders" || c.Provider.Instance != "orders-primary" {
		t.Fatalf("provider block: %+v", c.Provider)
	}
	if len(c.Auth.Scopes) != 1 || c.Auth.Scopes[0] != "https://www.googleapis.com/auth/alloydb.login" {
		t.Fatalf("alloydb gcp_iam auth must carry the alloydb.login scope, got %v", c.Auth.Scopes)
	}
	// Direct dial: the collector refuses verify-* on alloydb without a
	// ca_cert pin; `require` is its accepted default.
	if c.Connect.SSLMode != "require" {
		t.Fatalf("alloydb must render ssl_mode require, got %q", c.Connect.SSLMode)
	}
}

// The MySQL engine accepts only its own ssl_mode spellings.
func TestGcpComponent_MySQLGetsMySQLSSLModeSpellings(t *testing.T) {
	base := GcpTarget{
		ProviderType: "cloud_sql", Project: "p", Region: "r", InstanceID: "i",
		Engine: "mysql", Host: "h.", Port: 3306,
		AuthMethod: "password", User: "dbg_ro",
	}
	if c := gcpComponent(base); c.Connect.SSLMode != "verify_identity" {
		t.Fatalf("mysql spelling for verify-full: %q", c.Connect.SSLMode)
	}
	internal := base
	internal.ServerCaMode = "GOOGLE_MANAGED_INTERNAL_CA"
	if c := gcpComponent(internal); c.Connect.SSLMode != "required" {
		t.Fatalf("mysql spelling for require on the internal CA: %q", c.Connect.SSLMode)
	}
	pg := base
	pg.Engine = "postgres"
	if c := gcpComponent(pg); c.Connect.SSLMode != "verify-full" {
		t.Fatalf("postgres keeps libpq spellings: %q", c.Connect.SSLMode)
	}
}

func TestGcpComponent_CloudSQLCarriesNoScopes(t *testing.T) {
	c := gcpComponent(GcpTarget{
		ProviderType: "cloud_sql", Project: "p", Region: "r", InstanceID: "i",
		Engine: "postgres", Host: "h.", Port: 5432, AuthMethod: "gcp_iam", User: "u",
	})
	if len(c.Auth.Scopes) != 0 {
		t.Fatalf("cloud_sql auth needs no extra scopes, got %v", c.Auth.Scopes)
	}
}

// Every key this CLI renders must survive StrictParseConfig.
func TestGcpConfigTOML_RoundTripsThroughTheStrictParser(t *testing.T) {
	rendered, err := GcpConfigTOML("agent123", "tenant123", []GcpTarget{
		{
			ProviderType: "cloud_sql", Project: "p", Region: "r", InstanceID: "i",
			Engine: "postgres", Host: "h.", Port: 5432, AuthMethod: "gcp_iam", User: "u",
		},
		{
			ProviderType: "alloydb", Project: "p", Region: "r", ClusterID: "c",
			InstanceID: "c-primary", Engine: "postgres", Host: "10.0.0.1", Port: 5432,
			AuthMethod: "password", User: "postgres",
		},
		{
			ProviderType: "alloydb", Project: "p", Region: "r", ClusterID: "c2",
			InstanceID: "c2-primary", Engine: "postgres", Host: "10.0.0.2", Port: 5432,
			AuthMethod: "gcp_iam", User: "u",
		},
	}, Endpoints{}, true)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err := StrictParseConfig(rendered)
	if err != nil {
		t.Fatalf("the strict parser must model every key this CLI renders: %v", err)
	}
	if len(parsed.Component) != 3 {
		t.Fatalf("components: %+v", parsed.Component)
	}
	if parsed.Component[0].Provider.Project != "p" || parsed.Component[1].Provider.Cluster != "c" {
		t.Fatalf("gcp provider keys did not round-trip: %+v", parsed.Component)
	}
	if s := parsed.Component[2].Auth.Scopes; len(s) != 1 || s[0] != "https://www.googleapis.com/auth/alloydb.login" {
		t.Fatalf("the scopes key did not round-trip: %+v", parsed.Component[2].Auth)
	}
	if strings.Contains(rendered, "tenant123secret") || !strings.Contains(rendered, "${DBG_SERVER_SECRET}") {
		t.Fatalf("secrets must stay env references:\n%s", rendered)
	}
}

// MySQL spells the IAM flag with underscores.
func TestMergeCloudSQLInstance_MySQLIamFlagUnderscoreForm(t *testing.T) {
	var info sqlInstanceInfo
	info.Name = "prod-my"
	info.Region = "us-central1"
	info.DatabaseVersion = "MYSQL_8_0"
	info.Settings.DatabaseFlags = []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{{Name: "cloudsql_iam_authentication", Value: "On"}}

	got := mergeCloudSQLInstance(GcpTarget{Project: "p"}, &info)
	if got.Engine != "mysql" {
		t.Fatalf("engine: %+v", got)
	}
	if !got.IamEnabled {
		t.Fatalf("underscore-form IAM flag not detected: %+v", got)
	}
}

// The GCP grant script has no CREATE USER (gcloud registers the user) and no
// rds_iam (an RDS role); either would abort a paste run as one transaction.
func TestGcpGrantStatements(t *testing.T) {
	got := GcpGrantStatements("collector@p.iam", []string{"app"})
	want := []string{
		`GRANT pg_monitor TO "collector@p.iam";`,
		`GRANT CONNECT ON DATABASE "app" TO "collector@p.iam";`,
		`GRANT pg_read_all_data TO "collector@p.iam";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GcpGrantStatements:\n got  %q\n want %q", got, want)
	}
	for _, s := range got {
		if strings.Contains(s, "rds_iam") || strings.Contains(s, "CREATE USER") {
			t.Fatalf("RDS-only statement in the GCP script: %s", s)
		}
	}
}
