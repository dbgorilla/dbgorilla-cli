package collector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Where the collector's Terraform template for the gcp target is published.
//
// Same discipline as the aws target's CloudFormation template: the template is
// static — every input, the monitored databases included, is a Terraform input
// variable — so one published copy serves the CLI, a direct
// `gcloud infra-manager deployments apply`, and a customer's security review.
// It is the only copy: the CLI embeds nothing, so the files a customer reads
// at this address are exactly the files their project deploys.
const (
	// gcpTemplateBucket is named on its own in the egress-blocked error
	// message. Until a version is published there, --template-source points a
	// deploy at any gs:// copy of internal/collector/terraform/collector-gce.
	gcpTemplateBucket = "dbgorilla-collector-templates"

	gcpTemplateBase = "gs://" + gcpTemplateBucket + "/collector/gce/"
)

// GcpTemplateVersion is the template's OWN version, independent of the CLI
// release, bumped only when its input-variable contract changes — the same
// rules as TemplateVersion for the aws target, for the same reasons.
const GcpTemplateVersion = "v1.0"

// gcpTemplateProbeTimeout bounds the reachability check; it runs before every
// deploy and must fail fast with a named remediation rather than stall.
const gcpTemplateProbeTimeout = 5 * time.Second

// HostedGcpTemplateSource returns the published template directory this build
// deploys — the default for an install's --template-source.
func HostedGcpTemplateSource() string { return gcpTemplateBase + GcpTemplateVersion }

// probeGcpTemplate confirms the published template is reachable before any
// mutation. Infrastructure Manager reads the gs:// source itself, but probing
// here turns "the deploy failed twenty minutes in" into "your egress or the
// template address is wrong" upfront.
func probeGcpTemplate(ctx context.Context, cfg gcpConfig, source string) error {
	rest, ok := strings.CutPrefix(source, "gs://")
	if !ok {
		return fmt.Errorf("template source %q is not a gs:// address", source)
	}
	probeURL := "https://storage.googleapis.com/" + strings.TrimSuffix(rest, "/") + "/main.tf"

	probeCtx, cancel := context.WithTimeout(ctx, gcpTemplateProbeTimeout)
	defer cancel()
	resp, err := gcpSend(probeCtx, cfg, http.MethodGet, probeURL, "", nil)
	if err != nil {
		return fmt.Errorf("could not reach the published collector template at %s "+
			"(check egress to storage.googleapis.com, or pass --template-source): %w",
			probeURL, err)
	}
	resp.Body.Close()
	return nil
}
