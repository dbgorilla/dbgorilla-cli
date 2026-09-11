package collector

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The template's input contract and version live in the Go constants the CLI
// deploys with and in the published Terraform files; these pins keep the two
// together.

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
	contract := append(append([]string{}, gcpInputKeys...), GcpSecretInputKeys...)
	sort.Strings(contract)
	if strings.Join(declared, ",") != strings.Join(contract, ",") {
		t.Fatalf("template variables %v != the CLI's input keys %v — bump the template contract",
			declared, contract)
	}
}

func TestGcpTemplateContract_NamingContractHolds(t *testing.T) {
	sa := GcpRuntimeServiceAccountFor("dbgorilla-collector", "acme-prod")
	if sa != "dbgorilla-collector@acme-prod.iam.gserviceaccount.com" {
		t.Fatalf("service-account naming contract changed: %s", sa)
	}
	main, err := os.ReadFile("terraform/collector-gce/main.tf")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	for _, want := range []string{"name               = local.name", "base_instance_name = local.name"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("the MIG and its instances must carry the deployment name (%q missing)", want)
		}
	}
	if got := migPath("acme-prod", "us-central1", "dbgorilla-collector"); !strings.HasSuffix(got, "/instanceGroupManagers/dbgorilla-collector") {
		t.Errorf("day-2 helpers must address the MIG by the deployment name, got %s", got)
	}
}

// What `dbg collector logs` and the boot sequence rely on in the template.
func TestGcpTemplateContract_RuntimePins(t *testing.T) {
	main, err := os.ReadFile("terraform/collector-gce/main.tf")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	for _, want := range []string{
		`google-logging-enabled  = "true"`,
		`--name ` + gcpCollectorContainerName,
		`subnetwork = var.subnetwork == "" ? null : var.subnetwork`,
		"depends_on = [",
	} {
		if !strings.Contains(string(main), want) {
			t.Errorf("main.tf must contain %q", want)
		}
	}
}

func TestGcpDeployInputs_RendersTheFullContract(t *testing.T) {
	inputs, secrets, err := GcpDeployInputs(GcpStackInput{
		AgentID: "agent123", TenantID: "tenant123",
		Image: "example.registry/collector@sha256:abc",
		Targets: []GcpTarget{{
			ProviderType: "cloud_sql", Project: "p", Region: "us-central1",
			InstanceID: "orders-pg", Engine: "postgres", Host: "h.", Port: 5432,
			AuthMethod: "gcp_iam", User: "u",
		}},
		Network:         "projects/p/global/networks/default",
		Region:          "us-central1",
		DeploymentName:  "dbgorilla-collector",
		Project:         "p",
		ServerSecret:    "secret123",
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
	// Secrets live in their own map, never beside the printable inputs.
	var skeys []string
	for k := range secrets {
		skeys = append(skeys, k)
	}
	sort.Strings(skeys)
	if strings.Join(skeys, ",") != strings.Join(GcpSecretInputKeys, ",") {
		t.Fatalf("rendered secrets %v != contract %v", skeys, GcpSecretInputKeys)
	}
	if secrets["server_secret"] != "secret123" {
		t.Fatal("the server secret must ride the secrets map")
	}
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
