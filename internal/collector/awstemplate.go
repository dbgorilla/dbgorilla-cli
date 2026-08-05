package collector

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

// Where the collector's CloudFormation template is published.
//
// The template is static — every input, the monitored databases included, is a
// stack parameter — so one published copy serves the CLI, a `TemplateURL`
// deploy, and a console quick-create link. It is the only copy: the CLI does not
// embed one, so the file a customer reads and security-reviews at this URL is
// exactly the file their account deploys.
//
// CloudFormation only accepts S3 URLs for TemplateURL, so this is the canonical
// address; a vanity domain would not work here.
const (
	// templateHost is named on its own in the egress-blocked error message.
	templateHost = "dbgorilla-cfn-us-east-1.s3.us-east-1.amazonaws.com"

	templateBaseURL = "https://" + templateHost + "/collector/fargate/"

	// QuickCreateBaseURL launches the hosted template in the AWS console, for
	// customers who would rather not run the CLI against their own account.
	QuickCreateBaseURL = "https://console.aws.amazon.com/cloudformation/home#/stacks/quickcreate"
)

// templateProbeTimeout bounds the reachability check. It runs before every
// deploy, so it must fail fast and report a blocked egress path rather than
// stall the install behind it.
const templateProbeTimeout = 5 * time.Second

// TemplateVersion is the Fargate template's OWN version, deliberately
// independent of the CLI's release version.
//
// The two change at completely different rates. The template's contract is its
// parameter set, and most CLI releases do not touch it — tying them together
// meant republishing an identical file under a new key on every release, and
// meant a CLI could not deploy at all unless a template had been published under
// its exact version. Versioned on its own, the template moves only when its
// contract does, and every CLI that speaks v1.0's parameters deploys v1.0.
//
// Bump this only for a change to that contract: a parameter added, removed, or
// reinterpreted. Publishing refuses to overwrite an existing version with
// different content, so a forgotten bump fails CI rather than silently changing
// what already-released CLIs deploy.
//
// The same version is recorded in the template itself
// (Metadata.DBGorilla.TemplateVersion) — that is what CI publishes under, and
// TestTemplateVersionMatches keeps the two from drifting.
const TemplateVersion = "v1.0"

// hostedTemplateURL is the published template this build deploys.
var hostedTemplateURL = templateBaseURL + TemplateVersion + ".yaml"

// HostedTemplateURL returns the published template URL this build deploys, for
// `dbg doctor` and the dry-run output.
func HostedTemplateURL() string { return hostedTemplateURL }

// templateRef is how one stack operation gets its template: a published URL, or
// the stack's existing template (an in-place update that must not swap it).
type templateRef struct {
	URL                 string
	UsePreviousTemplate bool
}

// resolveTemplate decides what a deploy sends to CloudFormation: always a
// published template URL, never a local copy.
//
// There is no embedded fallback by design. One published file per template
// version is the single source of truth — what a customer reviews at the URL is
// exactly what their account deploys, and there is no second copy that could
// drift from it or be deployed without having been published. The cost is that a
// CLI that cannot reach S3 cannot deploy at all; that fails loudly here rather
// than quietly deploying something else.
func resolveTemplate(ctx context.Context, override string) (templateRef, error) {
	if override != "" {
		if err := probeTemplate(ctx, override); err != nil {
			return templateRef{}, fmt.Errorf("template %s is not reachable: %w", override, err)
		}
		return templateRef{URL: override}, nil
	}
	if err := probeTemplate(ctx, hostedTemplateURL); err != nil {
		return templateRef{}, fmt.Errorf(
			"could not fetch the collector's CloudFormation template (%s) at %s: %w\n\n"+
				"If this build of dbg expects a template version that was never published, "+
				"update it with 'dbg upgrade' and re-run.\n"+
				"If you are behind a proxy or restricted egress, allow HTTPS to %s, "+
				"or pass a template you host yourself with --template-url",
			TemplateVersion, hostedTemplateURL, err, templateHost)
	}
	return templateRef{URL: hostedTemplateURL}, nil
}

// probeTemplate reports whether a template URL is fetchable right now. A HEAD is
// enough: CloudFormation fetches the body itself, so the CLI only needs to know
// the URL resolves before handing it over — and can say so in its own words.
func probeTemplate(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, templateProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

// applyCreate sets the template fields on a CreateStack call.
func (t templateRef) applyCreate(in *cloudformation.CreateStackInput) {
	in.TemplateURL = aws.String(t.URL)
}

// applyUpdate sets the template fields on an UpdateStack call. Reusing the
// stack's current template wins over both, so an image upgrade cannot rewrite
// the template underneath a running collector.
func (t templateRef) applyUpdate(in *cloudformation.UpdateStackInput) {
	if t.UsePreviousTemplate {
		in.UsePreviousTemplate = aws.Bool(true)
		return
	}
	in.TemplateURL = aws.String(t.URL)
}

// applyValidate sets the template fields on a ValidateTemplate call (dry run).
func (t templateRef) applyValidate(in *cloudformation.ValidateTemplateInput) {
	in.TemplateURL = aws.String(t.URL)
}

// Source describes where the template came from, for dry-run/status output.
func (t templateRef) Source() string { return t.URL }
