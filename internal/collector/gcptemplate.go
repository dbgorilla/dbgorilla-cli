package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Where the collector's Terraform template for the gcp target is published.
// The CLI embeds no copy: what a customer reads at this address is exactly
// what their project deploys.
const (
	gcpTemplateBucket = "dbgorilla-collector-templates"
	gcpTemplateBase   = "gs://" + gcpTemplateBucket + "/collector/gce/"
)

// GcpTemplateVersion is the template's own version, bumped when its input
// contract changes; a published version is never rewritten. v1.3 removed the
// secret inputs: the CLI writes them to Secret Manager itself. v1.4 both added
// a fourth secret, <name>-prometheus-api-key, to the boot script's fetch/export
// set (the optional Instaclustr Prometheus key), and scoped the IAM grants per
// database service, conditioning Cloud SQL's login role on the monitored
// instances. Both land in v1.4 because neither has been published yet; once a
// version is out, the next change takes a new one.
const GcpTemplateVersion = "v1.4"

const gcpTemplateProbeTimeout = 5 * time.Second

// HostedGcpTemplateSource returns the published template directory this build
// deploys.
func HostedGcpTemplateSource() string { return gcpTemplateBase + GcpTemplateVersion }

// probeGcpTemplate confirms the template is reachable before any mutation,
// and — when its main.tf carries the `# template-version:` marker every
// published version does — that it declares the contract version this CLI
// deploys. Without the check, a --template-source pinned to another version
// fails only after everything is provisioned, or not visibly at all: the CLI
// writes this version's secret set and renders a config referencing it, and
// a boot script from another version fetches a different set (an older one
// silently strands the Prometheus key; a newer one blocks the boot waiting
// on a secret this CLI never wrote). A template without the marker (a fork
// the operator owns) is deployed as given.
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
	defer func() { _ = resp.Body.Close() }()
	main, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Reachability is already proven; an unreadable body only forfeits
		// the version check.
		return nil
	}
	return checkGcpTemplateVersion(source, main)
}

var gcpTemplateVersionMarker = regexp.MustCompile(`(?m)^# template-version: (\S+)$`)

// checkGcpTemplateVersion refuses a template that declares a contract version
// other than the one this CLI speaks. Absent marker = no check.
func checkGcpTemplateVersion(source string, mainTF []byte) error {
	m := gcpTemplateVersionMarker.FindSubmatch(mainTF)
	if m == nil || string(m[1]) == GcpTemplateVersion {
		return nil
	}
	return fmt.Errorf("the template at %s declares template-version %s, but this CLI deploys the %s contract "+
		"(the secrets it writes and the config it renders match %s only). "+
		"Host a copy of the %s template there, or drop --template-source to deploy the published one",
		source, m[1], GcpTemplateVersion, GcpTemplateVersion, GcpTemplateVersion)
}

// GcpTemplateSourceVersion is the version a template source deploys — the
// final path segment of the published layout (…/collector/gce/v1.3).
func GcpTemplateSourceVersion(source string) string {
	return lastPathSegment(strings.TrimSuffix(source, "/"))
}

// CompareGcpTemplateVersions orders two "vMAJOR.MINOR" template versions.
// ok is false when either is not of that form (a custom --template-source).
func CompareGcpTemplateVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseGcpTemplateVersion(a)
	pb, okb := parseGcpTemplateVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

func parseGcpTemplateVersion(v string) (parts [2]int, ok bool) {
	rest, ok := strings.CutPrefix(v, "v")
	if !ok {
		return parts, false
	}
	major, minor, ok := strings.Cut(rest, ".")
	if !ok {
		return parts, false
	}
	var err error
	if parts[0], err = strconv.Atoi(major); err != nil {
		return parts, false
	}
	if parts[1], err = strconv.Atoi(minor); err != nil {
		return parts, false
	}
	return parts, true
}
