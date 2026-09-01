package collector

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The gcp target: Cloud SQL (Postgres/MySQL) and AlloyDB (Postgres), monitored
// by a collector deployed inside the customer's project. Discovery mirrors the
// aws target's shape — list the supported databases, auto-select a solo
// candidate, refuse to guess between several — against the Cloud SQL Admin and
// AlloyDB APIs instead of RDS.

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
	User       string
	AuthMethod string // "gcp_iam" | "password"
	Commands   []string
}

// Complete reports whether discovery still needs to fill anything in.
func (t GcpTarget) Complete() bool {
	if t.Project == "" || t.Region == "" || t.InstanceID == "" || t.Host == "" {
		return false
	}
	if t.ProviderType == "alloydb" && t.ClusterID == "" {
		return false
	}
	return t.ProviderType != ""
}

// GcpTargetChoice is one selectable database, for the interactive picker.
type GcpTargetChoice struct {
	ID           string // cloud_sql instance id, or "cluster/instance" for alloydb
	ProviderType string
}

// AmbiguousGcpTargetError reports that the project holds several supported
// databases, carrying the candidates so an interactive caller can present a
// choice. Mirrors AmbiguousTargetError for aws: selection never guesses.
type AmbiguousGcpTargetError struct {
	Instances []string // Cloud SQL instance ids
	Clusters  []string // AlloyDB "cluster/primary-instance" ids
}

func (e *AmbiguousGcpTargetError) Error() string {
	return fmt.Sprintf("found multiple databases (Cloud SQL %v, AlloyDB %v); pass --db-instance-id to choose one",
		e.Instances, e.Clusters)
}

// Candidates returns the choices in a stable order: Cloud SQL instances, then
// AlloyDB clusters.
func (e *AmbiguousGcpTargetError) Candidates() []GcpTargetChoice {
	out := make([]GcpTargetChoice, 0, len(e.Instances)+len(e.Clusters))
	for _, id := range e.Instances {
		out = append(out, GcpTargetChoice{ID: id, ProviderType: "cloud_sql"})
	}
	for _, id := range e.Clusters {
		out = append(out, GcpTargetChoice{ID: id, ProviderType: "alloydb"})
	}
	return out
}

// selectSoloGcp picks the target when exactly one candidate exists, returns a
// plain error on zero, and *AmbiguousGcpTargetError on more than one. Pure.
func selectSoloGcp(instances, clusters []string) (id, kind string, err error) {
	switch len(instances) + len(clusters) {
	case 0:
		return "", "", fmt.Errorf("no Cloud SQL or AlloyDB databases found in this project; " +
			"pass --db-instance-id (Cloud SQL instance, or alloydb cluster/instance)")
	case 1:
		if len(instances) == 1 {
			return instances[0], "cloud_sql", nil
		}
		return clusters[0], "alloydb", nil
	default:
		return "", "", &AmbiguousGcpTargetError{Instances: instances, Clusters: clusters}
	}
}

// DiscoverGcpTarget completes `into` from the control plane. `id` may be empty
// (solo-select), a Cloud SQL instance id, or an alloydb "cluster" or
// "cluster/instance". `providerHint` forces cloud_sql vs alloydb when both
// carry the same id.
func DiscoverGcpTarget(id, providerHint string, into GcpTarget) (GcpTarget, error) {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return into, fmt.Errorf("could not load Google Cloud credentials "+
			"(run 'gcloud auth application-default login'): %w", err)
	}
	if into.Project == "" {
		return into, fmt.Errorf("no project resolved for discovery; pass --project")
	}

	if id == "" {
		instances, clusters, err := listGcpCandidates(ctx, cfg, into.Project, providerHint)
		if err != nil {
			return into, err
		}
		id, providerHint, err = selectSoloGcp(instances, clusters)
		if err != nil {
			return into, err
		}
	}

	switch {
	case providerHint == "alloydb" || strings.Contains(id, "/"):
		return discoverAlloyDB(ctx, cfg, into, id)
	default:
		return discoverCloudSQL(ctx, cfg, into, id)
	}
}

// listGcpCandidates enumerates the supported databases: Cloud SQL Postgres +
// MySQL primaries (replicas follow their primary and are never targets), and
// AlloyDB primary clusters as "cluster/primary". A provider hint narrows the
// listing so a hinted install doesn't require permissions on the other API.
func listGcpCandidates(ctx context.Context, cfg gcpConfig, project, providerHint string) (instances, clusters []string, err error) {
	if providerHint == "" || providerHint == "cloud_sql" {
		sqlInstances, err := listCloudSQLInstances(ctx, cfg, project)
		if err != nil {
			return nil, nil, err
		}
		for _, inst := range sqlInstances {
			if inst.MasterInstanceName != "" || !supportedCloudSQLEngine(inst.DatabaseVersion) {
				continue
			}
			instances = append(instances, inst.Name)
		}
	}
	if providerHint == "" || providerHint == "alloydb" {
		adbClusters, err := listAlloyDBPrimaries(ctx, cfg, project)
		if err != nil {
			return nil, nil, err
		}
		clusters = append(clusters, adbClusters...)
	}
	sort.Strings(instances)
	sort.Strings(clusters)
	return instances, clusters, nil
}

func supportedCloudSQLEngine(databaseVersion string) bool {
	return strings.HasPrefix(databaseVersion, "POSTGRES") ||
		strings.HasPrefix(databaseVersion, "MYSQL")
}

// --- Cloud SQL (sqladmin v1) -----------------------------------------------

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

func listCloudSQLInstances(ctx context.Context, cfg gcpConfig, project string) ([]sqlInstanceInfo, error) {
	var out []sqlInstanceInfo
	pageToken := ""
	for {
		u := fmt.Sprintf("https://sqladmin.googleapis.com/v1/projects/%s/instances", url.PathEscape(project))
		if pageToken != "" {
			u += "?pageToken=" + url.QueryEscape(pageToken)
		}
		var page struct {
			Items         []sqlInstanceInfo `json:"items"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := gcpGetJSON(ctx, cfg, u, &page); err != nil {
			return nil, fmt.Errorf("could not list Cloud SQL instances in project %q "+
				"(is the Cloud SQL Admin API enabled, and does this identity hold cloudsql.viewer?): %w",
				project, err)
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		pageToken = page.NextPageToken
	}
}

func getCloudSQLInstance(ctx context.Context, cfg gcpConfig, project, instance string) (*sqlInstanceInfo, error) {
	u := fmt.Sprintf("https://sqladmin.googleapis.com/v1/projects/%s/instances/%s",
		url.PathEscape(project), url.PathEscape(instance))
	var info sqlInstanceInfo
	if err := gcpGetJSON(ctx, cfg, u, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// discoverCloudSQL fills the target from one instances.get.
func discoverCloudSQL(ctx context.Context, cfg gcpConfig, into GcpTarget, instance string) (GcpTarget, error) {
	info, err := getCloudSQLInstance(ctx, cfg, into.Project, instance)
	if err != nil {
		return into, fmt.Errorf("no Cloud SQL instance named %q found in project %q — "+
			"check the project (--project) and pass an existing --db-instance-id, then re-run: %w",
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
		if f.Name == "cloudsql.iam_authentication" && strings.EqualFold(f.Value, "on") {
			into.IamEnabled = true
		}
	}
	return into
}

// --- AlloyDB (alloydb v1) ---------------------------------------------------

type alloydbInstanceInfo struct {
	Name         string `json:"name"` // full resource path
	InstanceType string `json:"instanceType"`
	State        string `json:"state"`
	IPAddress    string `json:"ipAddress"`
}

func (i alloydbInstanceInfo) shortID() string {
	parts := strings.Split(i.Name, "/")
	return parts[len(parts)-1]
}

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

// listAlloyDBPrimaries returns "cluster/primary-instance" ids across all
// locations (the aggregate `locations/-` listing).
func listAlloyDBPrimaries(ctx context.Context, cfg gcpConfig, project string) ([]string, error) {
	var clusters []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	pageToken := ""
	for {
		u := fmt.Sprintf("https://alloydb.googleapis.com/v1/projects/%s/locations/-/clusters", url.PathEscape(project))
		if pageToken != "" {
			u += "?pageToken=" + url.QueryEscape(pageToken)
		}
		var page struct {
			Clusters []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"clusters"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := gcpGetJSON(ctx, cfg, u, &page); err != nil {
			return nil, fmt.Errorf("could not list AlloyDB clusters in project %q "+
				"(is the AlloyDB API enabled, and does this identity hold alloydb.viewer?): %w",
				project, err)
		}
		clusters = append(clusters, page.Clusters...)
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	var out []string
	for _, c := range clusters {
		parts := strings.Split(c.Name, "/")
		clusterID := parts[len(parts)-1]
		location := alloydbLocation(c.Name)
		instances, err := listAlloyDBInstances(ctx, cfg, project, location, clusterID)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			if inst.InstanceType == "PRIMARY" {
				out = append(out, clusterID+"/"+inst.shortID())
			}
		}
	}
	return out, nil
}

func listAlloyDBInstances(ctx context.Context, cfg gcpConfig, project, location, cluster string) ([]alloydbInstanceInfo, error) {
	u := fmt.Sprintf("https://alloydb.googleapis.com/v1/projects/%s/locations/%s/clusters/%s/instances",
		url.PathEscape(project), url.PathEscape(location), url.PathEscape(cluster))
	var page struct {
		Instances []alloydbInstanceInfo `json:"instances"`
	}
	if err := gcpGetJSON(ctx, cfg, u, &page); err != nil {
		return nil, fmt.Errorf("could not list instances of AlloyDB cluster %q: %w", cluster, err)
	}
	return page.Instances, nil
}

// discoverAlloyDB fills the target from the cluster's instance listing. `id`
// is "cluster" (primary auto-picked) or "cluster/instance".
func discoverAlloyDB(ctx context.Context, cfg gcpConfig, into GcpTarget, id string) (GcpTarget, error) {
	clusterID, instanceID, _ := strings.Cut(id, "/")
	if into.Region == "" {
		region, err := findAlloyDBClusterRegion(ctx, cfg, into.Project, clusterID)
		if err != nil {
			return into, err
		}
		into.Region = region
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
	u := fmt.Sprintf("https://alloydb.googleapis.com/v1/projects/%s/locations/-/clusters", url.PathEscape(project))
	var page struct {
		Clusters []struct {
			Name string `json:"name"`
		} `json:"clusters"`
	}
	if err := gcpGetJSON(ctx, cfg, u, &page); err != nil {
		return "", fmt.Errorf("could not list AlloyDB clusters in project %q: %w", project, err)
	}
	for _, c := range page.Clusters {
		parts := strings.Split(c.Name, "/")
		if parts[len(parts)-1] == clusterID {
			return alloydbLocation(c.Name), nil
		}
	}
	return "", fmt.Errorf("no AlloyDB cluster named %q found in project %q — "+
		"check the project (--project) and the cluster id, then re-run", clusterID, project)
}

// --- config rendering --------------------------------------------------------

// GcpDBPasswordEnv is the password reference for the gcp target. Like the aws
// target, the deployment names the variable itself (fed from Secret Manager).
const GcpDBPasswordEnv = "DBG_DB_PASSWORD"

// gcpComponent lowers one target into its [[component]] block.
func gcpComponent(t GcpTarget) Component {
	name := t.InstanceID
	if t.ProviderType == "alloydb" {
		name = t.ClusterID
	}
	provider := Provider{
		Type:     t.ProviderType,
		Project:  t.Project,
		Region:   t.Region,
		Instance: t.InstanceID,
	}
	if t.ProviderType == "alloydb" {
		provider.Cluster = t.ClusterID
	}
	auth := Auth{Method: t.AuthMethod, User: t.User}
	if auth.Method == "" {
		auth.Method = "gcp_iam"
	}
	if auth.User == "" {
		auth.User = "dbgorilla"
	}
	if auth.Method == "password" {
		auth.Password = "${" + GcpDBPasswordEnv + "}"
	}
	sslMode := "verify-full"
	switch {
	case t.ProviderType == "alloydb":
		// Every alloydb connection rides the collector's in-process connector
		// tunnel; the collector owns the transport, so the connect block's
		// ssl_mode is not the identity mechanism there.
		sslMode = "verify-full"
	case t.AuthMethod != "gcp_iam" && t.ServerCaMode == "GOOGLE_MANAGED_INTERNAL_CA":
		// Password auth on the default per-instance CA verifies the chain
		// against that CA (fetched by the collector at discovery): verify-ca.
		sslMode = "verify-ca"
	}
	return Component{
		Name:     name,
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
	cfg := Config{
		Dbgorilla: Dbgorilla{
			AgentID:      agentID,
			TenantID:     tenantID,
			Secret:       "${" + SecretEnv + "}",
			OpampBaseURL: eps.OpampBaseURL,
			OtlpBaseURL:  eps.OtlpBaseURL,
			AuthBaseURL:  eps.AuthBaseURL,
		},
		Topology: Topology{Interval: "60s"},
		Commands: Commands{Enabled: commandsEnabled},
	}
	for _, t := range targets {
		cfg.Component = append(cfg.Component, gcpComponent(t))
	}
	return cfg.Render()
}
