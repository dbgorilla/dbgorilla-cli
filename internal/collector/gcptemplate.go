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
	// message. NOTE: the bucket is provisioned by the release pipeline; until
	// that lands, --template-source overrides this for testing.
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
// deploys, for `dbg doctor` and the dry-run output.
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
	probeURL := "https://storage.googleapis.com/" + rest
	if !strings.HasSuffix(probeURL, "/") {
		probeURL += "/"
	}
	probeURL += "main.tf"

	probeCtx, cancel := context.WithTimeout(ctx, gcpTemplateProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	resp, err := cfg.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the published collector template at %s "+
			"(check egress to storage.googleapis.com, or pass --template-source): %w",
			probeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the published collector template at %s returned HTTP %d "+
			"(check the address, or pass --template-source)", probeURL, resp.StatusCode)
	}
	return nil
}
