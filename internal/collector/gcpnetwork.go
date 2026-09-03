package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ResolveGcpSubnetwork picks the subnetwork the collector instance joins in
// `region` on `network` (a projects/<p>/global/networks/<n> path). An
// auto-mode VPC needs none and yields "". A custom-mode VPC with exactly one
// subnetwork in the region yields it; otherwise the caller must pass
// --subnetwork.
func ResolveGcpSubnetwork(network, region string) (string, error) {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return "", gcpCredsErr(err)
	}
	project := alloydbPathSegment(network, "projects")
	name := lastPathSegment(network)
	if project == "" || name == "" {
		return "", fmt.Errorf("network %q is not a projects/<project>/global/networks/<name> path", network)
	}

	var net struct {
		AutoCreateSubnetworks bool `json:"autoCreateSubnetworks"`
	}
	u := fmt.Sprintf("%s/projects/%s/global/networks/%s", computeBase, url.PathEscape(project), url.PathEscape(name))
	if err := gcpDo(ctx, cfg, http.MethodGet, u, nil, &net); err != nil {
		return "", fmt.Errorf("could not read VPC %q (pass --subnetwork to skip the lookup): %w", network, err)
	}
	if net.AutoCreateSubnetworks {
		return "", nil
	}

	type subnetwork struct {
		Name     string `json:"name"`
		Network  string `json:"network"`
		SelfLink string `json:"selfLink"`
	}
	type page struct {
		gcpPage
		Items []subnetwork `json:"items"`
	}
	var matches []subnetwork
	u = fmt.Sprintf("%s/projects/%s/regions/%s/subnetworks", computeBase, url.PathEscape(project), url.PathEscape(region))
	err = gcpListPages(ctx, cfg, u, func(p page) {
		for _, s := range p.Items {
			if strings.HasSuffix(s.Network, "/"+strings.TrimPrefix(network, "/")) {
				matches = append(matches, s)
			}
		}
	})
	if err != nil {
		return "", fmt.Errorf("could not list subnetworks of VPC %q in %s (pass --subnetwork to skip the lookup): %w", network, region, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("VPC %q has no subnetwork in %s; create one there, or pass --subnetwork", network, region)
	case 1:
		return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", project, region, matches[0].Name), nil
	}
	names := make([]string, 0, len(matches))
	for _, s := range matches {
		names = append(names, s.Name)
	}
	return "", fmt.Errorf("VPC %q has several subnetworks in %s (%s); pass --subnetwork to choose one",
		network, region, strings.Join(names, ", "))
}
