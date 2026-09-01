package collector

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The template's input contract and version live in two places each — the Go
// constants the CLI deploys with, and the Terraform files that get published.
// These pins are what turns a forgotten bump into a test failure instead of a
// silent behavior change for already-released CLIs (the aws target's
// TestTemplateVersionMatches, ported).

func TestGcpTemplateContract_VersionMatches(t *testing.T) {
	main, err := os.ReadFile("terraform/collector-gce/main.tf")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	marker := regexp.MustCompile(`(?m)^# template-version: (\S+)$`).FindSubmatch(main)
	if marker == nil {
		t.Fatal("main.tf must carry a '# template-version: <v>' marker")
	}
	if got := string(marker[1]); got != GcpTemplateVersion {
		t.Fatalf("template says %s, the CLI deploys %s — bump them together", got, GcpTemplateVersion)
	}
}

func TestGcpTemplateContract_VariablesMatchInputKeys(t *testing.T) {
	raw, err := os.ReadFile("terraform/collector-gce/variables.tf")
	if err != nil {
		t.Fatalf("read variables: %v", err)
	}
	var declared []string
	for _, m := range regexp.MustCompile(`(?m)^variable "([^"]+)"`).FindAllSubmatch(raw, -1) {
		declared = append(declared, string(m[1]))
	}
	sort.Strings(declared)
	if strings.Join(declared, ",") != strings.Join(gcpInputKeys, ",") {
		t.Fatalf("template variables %v != the CLI's input keys %v — bump the template contract",
			declared, gcpInputKeys)
	}
}

func TestGcpTemplateContract_NamingContractHolds(t *testing.T) {
	// The template derives every resource name from the runtime service
	// account's local part, which GcpRuntimeServiceAccountFor sets to the
	// deployment name — and GcpMigFor promises the MIG carries that name.
	sa := GcpRuntimeServiceAccountFor("dbgorilla-collector", "acme-prod")
	if sa != "dbgorilla-collector@acme-prod.iam.gserviceaccount.com" {
		t.Fatalf("service-account naming contract changed: %s", sa)
	}
	if GcpMigFor("dbgorilla-collector") != "dbgorilla-collector" {
		t.Fatal("MIG naming contract changed")
	}
}

func TestGcpDatabaseUserFor(t *testing.T) {
	sa := "dbgorilla-collector@acme-prod.iam.gserviceaccount.com"
	// Postgres (Cloud SQL and AlloyDB): the email minus the suffix.
	if got := GcpDatabaseUserFor(sa, "postgres"); got != "dbgorilla-collector@acme-prod.iam" {
		t.Fatalf("postgres IAM username: %q", got)
	}
	// MySQL: the part before the @ (verified live).
	if got := GcpDatabaseUserFor(sa, "mysql"); got != "dbgorilla-collector" {
		t.Fatalf("mysql IAM username: %q", got)
	}
}

func TestGcpDeployInputs_RendersTheFullContract(t *testing.T) {
	inputs, err := GcpDeployInputs(GcpStackInput{
		AgentID: "agent123", TenantID: "tenant123",
		Image: "example.registry/collector@sha256:abc",
		Targets: []GcpTarget{{
			ProviderType: "cloud_sql", Project: "p", Region: "us-central1",
			InstanceID: "orders-pg", Engine: "postgres", Host: "h.", Port: 5432,
			AuthMethod: "gcp_iam", User: "u",
		}},
		Network:        "projects/p/global/networks/default",
		Region:         "us-central1",
		DeploymentName: "dbgorilla-collector",
		Project:        "p",
		ServerSecret:   "secret123",
		CommandsEnabled: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var keys []string
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(gcpInputKeys, ",") {
		t.Fatalf("rendered inputs %v != contract %v", keys, gcpInputKeys)
	}
	// The config carries no secret values — they ride their own inputs.
	decoded, err := DecodeConfig(inputs["collector_config"])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(decoded, "secret123") {
		t.Fatal("the server secret leaked into the config document")
	}
	if !strings.Contains(decoded, "${DBG_SERVER_SECRET}") {
		t.Fatalf("config must reference the secret env var:\n%s", decoded)
	}
}
