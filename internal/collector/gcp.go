package collector

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The gcp target: Cloud SQL (Postgres/MySQL) and AlloyDB (Postgres), monitored
// by a collector deployed inside the customer's project. Discovery has the aws
// target's shape — list the supported databases, auto-select a solo candidate,
// refuse to guess between several — against the Cloud SQL Admin and AlloyDB
// APIs instead of RDS.

// GcpTarget describes one database the gcp collector will monitor. Provider
// type "cloud_sql" names an instance; "alloydb" names a cluster plus its
// PRIMARY instance (read pools are the collector's discovery job, not ours).
type GcpTarget struct {
	ProviderType string // "cloud_sql" | "alloydb"
	Project      string
	Region       string
	InstanceID   string // cloud_sql instance id, or the alloydb PRIMARY instance id
	ClusterID    string // alloydb only

	Engine string // "postgres" | "mysql" (alloydb is always postgres)
	// Host is what [component.connect] records: the certificate-attested DNS
	// name for Cloud SQL when the instance has one (verbatim, trailing dot
	// included — that is the form the certificate carries), else the private
	// IP. For alloydb it is informational: every connection rides the
	// collector's connector-protocol tunnel.
	Host string
	Port int

	// Cloud SQL facts the install branches on.
	ServerCaMode string // GOOGLE_MANAGED_INTERNAL_CA (default) | *_CAS_CA
	IamEnabled   bool   // cloudsql.iam_authentication flag (alloydb needs no flag)

	Network string // VPC self-link, for the deployment's network preflight

	Databases  []string
	User       string   // empty means DefaultDBUser
	AuthMethod string   // "gcp_iam" (default) | "password"
	Commands   []string // query-analysis commands allowed; empty means none
}

// DisplayName is what the component is called in DBGorilla: the cluster for
// alloydb (its primary is an implementation detail), else the instance.
func (t GcpTarget) DisplayName() string {
	if t.ProviderType == "alloydb" {
		return t.ClusterID
	}
	return t.InstanceID
}

// DiscoverGcpTarget completes `into` from the control plane. `id` may be empty
// (solo-select), a Cloud SQL instance id, or an alloydb "cluster" or
// "cluster/instance". `providerHint` forces cloud_sql vs alloydb when both
// carry the same id.
func DiscoverGcpTarget(id, providerHint string, into GcpTarget) (GcpTarget, error) {
	if into.Project == "" {
		return into, errors.New("no project resolved for discovery; pass --project")
	}
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return into, gcpCredsErr(err)
	}

	location := ""
	if id == "" {
		cands, err := listGcpCandidates(ctx, cfg, into.Project, providerHint)
		if err != nil {
			return into, err
		}
		choice, err := selectSolo(cands.choices, errors.New(
			"no Cloud SQL or AlloyDB databases found in this project; "+
				"pass --db-instance-id (Cloud SQL instance, or alloydb cluster/instance)"))
		if err != nil {
			return into, err
		}
		id, providerHint, location = choice.ID, choice.ProviderType, cands.locations[choice.ID]
	}

	if providerHint == "alloydb" || strings.Contains(id, "/") {
		return discoverAlloyDB(ctx, cfg, into, id, location)
	}
	return discoverCloudSQL(ctx, cfg, into, id)
}

// gcpCandidates is a listing of the supported databases: the selectable
// choices in a stable order (Cloud SQL instances, then AlloyDB clusters), plus
// the AlloyDB locations the listing learned, keyed by choice id, so a
// solo-selected cluster needs no second lookup to be found again.
type gcpCandidates struct {
	choices   []TargetChoice
	locations map[string]string
}

// listGcpCandidates enumerates the supported databases: Cloud SQL Postgres +
// MySQL primaries (replicas follow their primary and are never targets), and
// AlloyDB primary clusters as "cluster/primary". A provider hint narrows the
// listing so a hinted install doesn't require permissions on the other API.
func listGcpCandidates(ctx context.Context, cfg gcpConfig, project, providerHint string) (gcpCandidates, error) {
	out := gcpCandidates{locations: map[string]string{}}
	if providerHint == "" || providerHint == "cloud_sql" {
		sqlInstances, err := listCloudSQLInstances(ctx, cfg, project)
		if err != nil {
			return out, err
		}
		var ids []string
		for _, inst := range sqlInstances {
			if inst.MasterInstanceName != "" || !supportedCloudSQLEngine(inst.DatabaseVersion) {
				continue
			}
			ids = append(ids, inst.Name)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out.choices = append(out.choices, TargetChoice{ID: id, ProviderType: "cloud_sql"})
		}
	}
	if providerHint == "" || providerHint == "alloydb" {
		primaries, err := listAlloyDBPrimaries(ctx, cfg, project)
		if err != nil {
			return out, err
		}
		sort.Slice(primaries, func(i, j int) bool { return primaries[i].id() < primaries[j].id() })
		for _, p := range primaries {
			out.choices = append(out.choices, TargetChoice{ID: p.id(), ProviderType: "alloydb"})
			out.locations[p.id()] = p.location
		}
	}
	return out, nil
}

func supportedCloudSQLEngine(databaseVersion string) bool {
	return strings.HasPrefix(databaseVersion, "POSTGRES") ||
		strings.HasPrefix(databaseVersion, "MYSQL")
}

// --- Cloud SQL (sqladmin v1) -----------------------------------------------

const sqlAdminBase = "https://sqladmin.googleapis.com/v1"

type sqlInstanceInfo struct {
	Name               string `json:"name"`
	Region             string `json:"region"`
	DatabaseVersion    string `json:"databaseVersion"`
	MasterInstanceName string `json:"masterInstanceName"`
	IPAddresses        []struct {
		Type      string `json:"type"`
		IPAddress string `json:"ipAddress"`
	} `json:"ipAddresses"`
	DNSNames []struct {
		Name string `json:"name"`
	} `json:"dnsNames"`
	Settings struct {
		IPConfiguration struct {
			ServerCaMode   string `json:"serverCaMode"`
			PrivateNetwork string `json:"privateNetwork"`
		} `json:"ipConfiguration"`
		DatabaseFlags []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"databaseFlags"`
	} `json:"settings"`
}

type sqlInstancesPage struct {
	gcpPage
	Items []sqlInstanceInfo `json:"items"`
}

func listCloudSQLInstances(ctx context.Context, cfg gcpConfig, project string) ([]sqlInstanceInfo, error) {
	var out []sqlInstanceInfo
	u := fmt.Sprintf("%s/projects/%s/instances", sqlAdminBase, url.PathEscape(project))
	err := gcpListPages(ctx, cfg, u, func(p sqlInstancesPage) { out = append(out, p.Items...) })
	if err != nil {
		return nil, fmt.Errorf("could not list Cloud SQL instances in project %q "+
			"(is the Cloud SQL Admin API enabled, and does this identity hold cloudsql.viewer?): %w",
			project, err)
	}
	return out, nil
}

func getCloudSQLInstance(ctx context.Context, cfg gcpConfig, project, instance string) (*sqlInstanceInfo, error) {
	u := fmt.Sprintf("%s/projects/%s/instances/%s", sqlAdminBase,
		url.PathEscape(project), url.PathEscape(instance))
	var info sqlInstanceInfo
	if err := gcpDo(ctx, cfg, "GET", u, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// discoverCloudSQL fills the target from one instances.get.
func discoverCloudSQL(ctx context.Context, cfg gcpConfig, into GcpTarget, instance string) (GcpTarget, error) {
	info, err := getCloudSQLInstance(ctx, cfg, into.Project, instance)
	if err != nil {
		return into, fmt.Errorf("no Cloud SQL instance named %q found in project %q — "+
			"check the project (--project) and pass an existing --db-instance-id, then re-run "+
			"(for an AlloyDB cluster, pass --provider-type alloydb): %w",
			instance, into.Project, err)
	}
	if info.MasterInstanceName != "" {
		return into, fmt.Errorf("Cloud SQL instance %q is a read replica of %q — "+
			"pass the primary as --db-instance-id; the collector discovers replicas from it",
			instance, info.MasterInstanceName)
	}
	if !supportedCloudSQLEngine(info.DatabaseVersion) {
		return into, fmt.Errorf("Cloud SQL instance %q runs %s, which this collector does not support "+
			"(PostgreSQL and MySQL are supported)", instance, info.DatabaseVersion)
	}
	return mergeCloudSQLInstance(into, info), nil
}

// mergeCloudSQLInstance projects an instances.get response into the target.
// Pure — the field mapping is pinned by tests, because a dropped serverCaMode
// or a trimmed DNS name produces an install that deploys cleanly and then
// fails TLS verification.
func mergeCloudSQLInstance(into GcpTarget, info *sqlInstanceInfo) GcpTarget {
	into.ProviderType = "cloud_sql"
	into.InstanceID = info.Name
	if into.Region == "" {
		into.Region = info.Region
	}
	into.Engine = "postgres"
	into.Port = 5432
	if strings.HasPrefix(info.DatabaseVersion, "MYSQL") {
		into.Engine = "mysql"
		into.Port = 3306
	}
	// The certificate-attested DNS name wins, VERBATIM — the trailing dot is
	// part of what the certificate carries. Fall back to the private IP, then
	// any address.
	if into.Host == "" {
		for _, d := range info.DNSNames {
			if d.Name != "" {
				into.Host = d.Name
				break
			}
		}
	}
	if into.Host == "" {
		for _, ip := range info.IPAddresses {
			if ip.Type == "PRIVATE" && ip.IPAddress != "" {
				into.Host = ip.IPAddress
				break
			}
		}
	}
	if into.Host == "" {
		for _, ip := range info.IPAddresses {
			if ip.IPAddress != "" {
				into.Host = ip.IPAddress
				break
			}
		}
	}
	into.ServerCaMode = info.Settings.IPConfiguration.ServerCaMode
	if into.ServerCaMode == "" {
		// An absent mode is the pre-CAS default: the per-instance internal CA.
		into.ServerCaMode = "GOOGLE_MANAGED_INTERNAL_CA"
	}
	into.Network = info.Settings.IPConfiguration.PrivateNetwork
	for _, f := range info.Settings.DatabaseFlags {
		// Postgres spells the flag with a dot; MySQL flags cannot contain dots
		// and use cloudsql_iam_authentication. Checking only one form makes IAM
		// look disabled on the other engine, and the install then refuses with
		// advice to enable a flag that doesn't exist there.
		if (f.Name == "cloudsql.iam_authentication" || f.Name == "cloudsql_iam_authentication") &&
			strings.EqualFold(f.Value, "on") {
			into.IamEnabled = true
		}
	}
	return into
}

// --- AlloyDB (alloydb v1) ---------------------------------------------------

const alloydbBase = "https://alloydb.googleapis.com/v1"

type alloydbClusterInfo struct {
	Name string `json:"name"` // full resource path
	// Network is the cluster's VPC (projects/<p>/global/networks/<n>) — the
	// collector instance joins it, so discovery must surface it or every
	// AlloyDB install falsely demands --network.
	Network string `json:"network"`
}

func (c alloydbClusterInfo) shortID() string  { return lastPathSegment(c.Name) }
func (c alloydbClusterInfo) location() string { return alloydbLocation(c.Name) }

type alloydbInstanceInfo struct {
	Name         string `json:"name"` // full resource path
	InstanceType string `json:"instanceType"`
	State        string `json:"state"`
	IPAddress    string `json:"ipAddress"`
}

func (i alloydbInstanceInfo) shortID() string { return lastPathSegment(i.Name) }

// alloydbPrimary is one cluster's PRIMARY instance together with the location
// the listing saw it in. Kept structured rather than flattened into the
// "cluster/instance" id, so discovery of a solo-selected cluster does not
// have to re-list every cluster to recover the location it just dropped.
type alloydbPrimary struct {
	cluster, instance, location string
}

func (p alloydbPrimary) id() string { return p.cluster + "/" + p.instance }

// alloydbLocation extracts the region from a full resource path.
func alloydbLocation(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "locations" {
			return parts[i+1]
		}
	}
	return ""
}

type alloydbClustersPage struct {
	gcpPage
	Clusters []alloydbClusterInfo `json:"clusters"`
}

type alloydbInstancesPage struct {
	gcpPage
	Instances []alloydbInstanceInfo `json:"instances"`
}

// listAlloyDBClusters lists every cluster across locations (the aggregate
// `locations/-` listing).
func listAlloyDBClusters(ctx context.Context, cfg gcpConfig, project string) ([]alloydbClusterInfo, error) {
	var out []alloydbClusterInfo
	u := fmt.Sprintf("%s/projects/%s/locations/-/clusters", alloydbBase, url.PathEscape(project))
	err := gcpListPages(ctx, cfg, u, func(p alloydbClustersPage) { out = append(out, p.Clusters...) })
	if err != nil {
		return nil, fmt.Errorf("could not list AlloyDB clusters in project %q "+
			"(is the AlloyDB API enabled, and does this identity hold alloydb.viewer?): %w",
			project, err)
	}
	return out, nil
}

// listAlloyDBPrimaries returns every cluster's PRIMARY instance.
func listAlloyDBPrimaries(ctx context.Context, cfg gcpConfig, project string) ([]alloydbPrimary, error) {
	clusters, err := listAlloyDBClusters(ctx, cfg, project)
	if err != nil {
		return nil, err
	}
	var out []alloydbPrimary
	for _, c := range clusters {
		instances, err := listAlloyDBInstances(ctx, cfg, project, c.location(), c.shortID())
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			if inst.InstanceType == "PRIMARY" {
				out = append(out, alloydbPrimary{cluster: c.shortID(), instance: inst.shortID(), location: c.location()})
			}
		}
	}
	return out, nil
}

func listAlloyDBInstances(ctx context.Context, cfg gcpConfig, project, location, cluster string) ([]alloydbInstanceInfo, error) {
	var out []alloydbInstanceInfo
	u := fmt.Sprintf("%s/projects/%s/locations/%s/clusters/%s/instances", alloydbBase,
		url.PathEscape(project), url.PathEscape(location), url.PathEscape(cluster))
	err := gcpListPages(ctx, cfg, u, func(p alloydbInstancesPage) { out = append(out, p.Instances...) })
	if err != nil {
		return nil, fmt.Errorf("could not list instances of AlloyDB cluster %q: %w", cluster, err)
	}
	return out, nil
}

// discoverAlloyDB fills the target from the cluster's instance listing. `id`
// is "cluster" (primary auto-picked) or "cluster/instance"; `location` is the
// cluster's region when the caller already knows it, else it is looked up.
func discoverAlloyDB(ctx context.Context, cfg gcpConfig, into GcpTarget, id, location string) (GcpTarget, error) {
	clusterID, instanceID, _ := strings.Cut(id, "/")
	if into.Region == "" {
		into.Region = location
	}
	if into.Region == "" {
		region, err := findAlloyDBClusterRegion(ctx, cfg, into.Project, clusterID)
		if err != nil {
			return into, err
		}
		into.Region = region
	}
	if into.Network == "" {
		u := fmt.Sprintf("%s/projects/%s/locations/%s/clusters/%s", alloydbBase,
			url.PathEscape(into.Project), url.PathEscape(into.Region), url.PathEscape(clusterID))
		var cl alloydbClusterInfo
		if err := gcpDo(ctx, cfg, "GET", u, nil, &cl); err == nil {
			into.Network = cl.Network
		}
		// A failed read falls through to the existing --network requirement.
	}
	instances, err := listAlloyDBInstances(ctx, cfg, into.Project, into.Region, clusterID)
	if err != nil {
		return into, fmt.Errorf("no AlloyDB cluster named %q found in project %q — "+
			"check the project (--project) and pass an existing --db-instance-id "+
			"(cluster or cluster/instance), then re-run: %w", clusterID, into.Project, err)
	}
	var primary *alloydbInstanceInfo
	for i := range instances {
		inst := &instances[i]
		if instanceID != "" && inst.shortID() == instanceID {
			primary = inst
			break
		}
		if instanceID == "" && inst.InstanceType == "PRIMARY" {
			primary = inst
			break
		}
	}
	if primary == nil {
		return into, fmt.Errorf("AlloyDB cluster %q has no matching PRIMARY instance — "+
			"pass --db-instance-id as cluster/instance to name it", clusterID)
	}
	if primary.InstanceType != "PRIMARY" {
		return into, fmt.Errorf("AlloyDB instance %q is a %s — name the cluster's PRIMARY; "+
			"the collector discovers read pools from it", primary.shortID(), primary.InstanceType)
	}
	into.ProviderType = "alloydb"
	into.ClusterID = clusterID
	into.InstanceID = primary.shortID()
	into.Engine = "postgres"
	into.Port = 5432
	if into.Host == "" {
		into.Host = primary.IPAddress
	}
	// AlloyDB IAM auth needs no database flag (validated live); the connector
	// tunnel provides transport identity for both auth methods.
	into.IamEnabled = true
	return into, nil
}

// findAlloyDBClusterRegion locates a cluster by id across locations.
func findAlloyDBClusterRegion(ctx context.Context, cfg gcpConfig, project, clusterID string) (string, error) {
	clusters, err := listAlloyDBClusters(ctx, cfg, project)
	if err != nil {
		return "", err
	}
	for _, c := range clusters {
		if c.shortID() == clusterID {
			return c.location(), nil
		}
	}
	return "", fmt.Errorf("no AlloyDB cluster named %q found in project %q — "+
		"check the project (--project) and the cluster id, then re-run", clusterID, project)
}

// --- config rendering --------------------------------------------------------

// gcpComponent lowers one target into its [[component]] block.
func gcpComponent(t GcpTarget) Component {
	provider := Provider{
		Type:     t.ProviderType,
		Project:  t.Project,
		Region:   t.Region,
		Instance: t.InstanceID,
	}
	if t.ProviderType == "alloydb" {
		provider.Cluster = t.ClusterID
	}
	auth := Auth{Method: orDefault(t.AuthMethod, "gcp_iam"), User: orDefault(t.User, DefaultDBUser)}
	if auth.Method == "password" {
		auth.Password = "${" + CloudDBPasswordEnv + "}"
	}
	if auth.Method == "gcp_iam" && t.ProviderType == "alloydb" {
		auth.Scopes = []string{"https://www.googleapis.com/auth/alloydb.login"}
	}
	// verify-full everywhere, with one exception: password auth on Cloud SQL's
	// default per-instance CA, which attests no hostname — there the chain is
	// verified against that CA (fetched by the collector at discovery). AlloyDB
	// connections ride the collector's connector tunnel, which owns transport
	// identity, so the connect block's mode is not the identity mechanism there.
	//
	// KNOWN EDGE (deliberate, review 2026-09-02): an OLDER Cloud SQL instance
	// with IAM auth + the per-instance internal CA + no dnsNames gets an IP
	// host and verify-full, and the hostname check can then fail after deploy.
	// The exception is keyed on auth method rather than CA mode because token
	// auth requires a verifying transport; the recommended paths for such
	// instances are enabling a DNS name / CAS, or --db-password. Widening the
	// exception to CA-mode+host-form is a follow-up decision, not an accident.
	sslMode := "verify-full"
	if t.ProviderType == "cloud_sql" && auth.Method == "password" && t.ServerCaMode == "GOOGLE_MANAGED_INTERNAL_CA" {
		sslMode = "verify-ca"
	}
	return Component{
		Name:     t.DisplayName(),
		Engine:   t.Engine,
		Commands: t.Commands,
		Provider: provider,
		Auth:     auth,
		Connect: Connect{
			Host:      t.Host,
			Port:      t.Port,
			Databases: t.Databases,
			SSLMode:   sslMode,
		},
	}
}

// GcpConfigTOML renders the collector config for the gcp deployment. Secrets
// stay ${ENV} references; the real values ride the deployment's Secret Manager
// parameters, never this document.
func GcpConfigTOML(agentID, tenantID string, targets []GcpTarget, eps Endpoints, commandsEnabled bool) (string, error) {
	cfg := baseConfig(agentID, tenantID, eps, commandsEnabled)
	for _, t := range targets {
		cfg.Component = append(cfg.Component, gcpComponent(t))
	}
	return cfg.Render()
}
