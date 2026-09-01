package collector

import (
	"errors"
	"strings"
	"testing"
)

// --- solo-target selection ---------------------------------------------------

func TestSelectSoloGcp_PicksTheOnlyCandidate(t *testing.T) {
	id, kind, err := selectSoloGcp([]string{"prod-pg"}, nil)
	if err != nil || id != "prod-pg" || kind != "cloud_sql" {
		t.Fatalf("got (%q, %q, %v)", id, kind, err)
	}
	id, kind, err = selectSoloGcp(nil, []string{"orders/orders-primary"})
	if err != nil || id != "orders/orders-primary" || kind != "alloydb" {
		t.Fatalf("got (%q, %q, %v)", id, kind, err)
	}
}

func TestSelectSoloGcp_ZeroCandidatesIsAPlainActionableError(t *testing.T) {
	_, _, err := selectSoloGcp(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--db-instance-id") {
		t.Fatalf("error must name the flag to pass: %v", err)
	}
	var amb *AmbiguousGcpTargetError
	if errors.As(err, &amb) {
		t.Fatalf("zero candidates must not be ambiguous: %v", err)
	}
}

func TestSelectSoloGcp_ManyCandidatesIsTypedSoTheTUICanPrompt(t *testing.T) {
	_, _, err := selectSoloGcp([]string{"a", "b"}, []string{"c/c-primary"})
	var amb *AmbiguousGcpTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("expected *AmbiguousGcpTargetError, got %v", err)
	}
	choices := amb.Candidates()
	if len(choices) != 3 {
		t.Fatalf("candidates: %v", choices)
	}
	// Stable order: Cloud SQL instances first, then AlloyDB clusters.
	if choices[0].ProviderType != "cloud_sql" || choices[2].ProviderType != "alloydb" {
		t.Fatalf("candidate order changed: %v", choices)
	}
}

// --- Cloud SQL field mapping -------------------------------------------------

// A dropped serverCaMode or a trimmed DNS name produces an install that deploys
// cleanly and then fails TLS verification — these pins are the regression net.
func TestMergeCloudSQLInstance_FieldByField(t *testing.T) {
	info := &sqlInstanceInfo{
		Name:            "prod-pg",
		Region:          "us-central1",
		DatabaseVersion: "POSTGRES_16",
	}
	info.DNSNames = []struct {
		Name string `json:"name"`
	}{{Name: "abc.def.us-central1.sql-psa.goog."}}
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
	// The DNS name is what the server certificate attests — VERBATIM, trailing
	// dot included. Trimming it broke live verify-full against Go's literal
	// x509 hostname matching.
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
	// An absent serverCaMode is the pre-CAS default: the per-instance internal
	// CA. Reading it as anything else silently accepts token auth on a CA that
	// cannot attest a hostname.
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

func TestGcpComponent_PasswordOnInternalCAConnectsAtVerifyCA(t *testing.T) {
	// The default per-instance CA attests no hostname; password auth on it
	// verifies the chain against that CA instead of failing verify-full.
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
	if c.Connect.SSLMode != "verify-ca" {
		t.Fatalf("expected verify-ca on the internal CA, got %q", c.Connect.SSLMode)
	}
	if c.Auth.Password != "${"+GcpDBPasswordEnv+"}" {
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
}

// GcpConfigTOML must survive StrictParseConfig: UpdateComponents round-trips
// the stored config through the strict parser, so any key the Config model
// does not carry would turn every later update into a refusal.
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
	}, Endpoints{}, true)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err := StrictParseConfig(rendered)
	if err != nil {
		t.Fatalf("the strict parser must model every key this CLI renders: %v", err)
	}
	if len(parsed.Component) != 2 {
		t.Fatalf("components: %+v", parsed.Component)
	}
	if parsed.Component[0].Provider.Project != "p" || parsed.Component[1].Provider.Cluster != "c" {
		t.Fatalf("gcp provider keys did not round-trip: %+v", parsed.Component)
	}
	if strings.Contains(rendered, "tenant123secret") || !strings.Contains(rendered, "${DBG_SERVER_SECRET}") {
		t.Fatalf("secrets must stay env references:\n%s", rendered)
	}
}

func TestGcpTargetComplete(t *testing.T) {
	base := GcpTarget{ProviderType: "cloud_sql", Project: "p", Region: "r", InstanceID: "i", Host: "h"}
	if !base.Complete() {
		t.Fatalf("complete target read as incomplete: %+v", base)
	}
	missing := base
	missing.Host = ""
	if missing.Complete() {
		t.Fatal("a hostless target is not complete")
	}
	adb := GcpTarget{ProviderType: "alloydb", Project: "p", Region: "r", InstanceID: "i", Host: "h"}
	if adb.Complete() {
		t.Fatal("alloydb without a cluster is not complete")
	}
	adb.ClusterID = "c"
	if !adb.Complete() {
		t.Fatalf("complete alloydb target read as incomplete: %+v", adb)
	}
}
