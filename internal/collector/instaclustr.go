package collector

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// InstaclustrMonitorUser is the read-only role the install creates for the
// collector. Created via the cluster's default user (which has CREATEROLE and
// pg_monitor), so no support ticket and no superuser are involved.
const InstaclustrMonitorUser = "dbgorilla_monitor"

// InstaclustrTarget is one managed PostgreSQL cluster as the Cluster
// Management API describes it, flattened across data centres.
type InstaclustrTarget struct {
	ClusterID       string
	Name            string
	Status          string
	PostgresVersion string
	// CloudProvider / Region use Instaclustr's own spellings (AWS_VPC,
	// US_EAST_1) — they ride into the collector config verbatim.
	CloudProvider string
	Region        string
	// DefaultUserPassword is the icpostgresql password the API returns on the
	// cluster-detail call. Used transiently to create the monitoring role;
	// never persisted, never rendered into any config.
	DefaultUserPassword string
	Nodes               []InstaclustrNode
}

// InstaclustrNode is one addressable node.
type InstaclustrNode struct {
	ID             string
	PublicAddress  string
	PrivateAddress string
	Rack           string
}

// Host returns the node address for the chosen network side.
func (n InstaclustrNode) Host(private bool) string {
	if private {
		return n.PrivateAddress
	}
	return n.PublicAddress
}

// instaclustrClusterDetail mirrors the fields this CLI reads from
// GET /cluster-management/v2/resources/applications/postgresql/clusters/v2/{id}.
type instaclustrClusterDetail struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	PostgresqlVersion   string `json:"postgresqlVersion"`
	DefaultUserPassword string `json:"defaultUserPassword"`
	DataCentres         []struct {
		CloudProvider string `json:"cloudProvider"`
		Region        string `json:"region"`
		Nodes         []struct {
			ID             string  `json:"id"`
			PublicAddress  *string `json:"publicAddress"`
			PrivateAddress *string `json:"privateAddress"`
			Rack           *string `json:"rack"`
			DeletionTime   *string `json:"deletionTime"`
		} `json:"nodes"`
	} `json:"dataCentres"`
}

// DiscoverInstaclustrCluster fetches the cluster detail and flattens its
// nodes. Deleted and address-less nodes (mid-provision) are skipped.
func DiscoverInstaclustrCluster(ctx context.Context, creds InstaclustrCreds, clusterID string) (InstaclustrTarget, error) {
	var detail instaclustrClusterDetail
	path := "/cluster-management/v2/resources/applications/postgresql/clusters/v2/" + clusterID
	if err := icGetJSON(ctx, creds, path, &detail); err != nil {
		return InstaclustrTarget{}, err
	}
	t := InstaclustrTarget{
		ClusterID:           detail.ID,
		Name:                detail.Name,
		Status:              detail.Status,
		PostgresVersion:     detail.PostgresqlVersion,
		DefaultUserPassword: detail.DefaultUserPassword,
	}
	for _, dc := range detail.DataCentres {
		if t.CloudProvider == "" {
			t.CloudProvider = dc.CloudProvider
			t.Region = dc.Region
		}
		for _, n := range dc.Nodes {
			if n.DeletionTime != nil && *n.DeletionTime != "" {
				continue
			}
			node := InstaclustrNode{ID: n.ID}
			if n.PublicAddress != nil {
				node.PublicAddress = *n.PublicAddress
			}
			if n.PrivateAddress != nil {
				node.PrivateAddress = *n.PrivateAddress
			}
			if n.Rack != nil {
				node.Rack = *n.Rack
			}
			if node.PublicAddress == "" && node.PrivateAddress == "" {
				continue
			}
			t.Nodes = append(t.Nodes, node)
		}
	}
	if len(t.Nodes) == 0 {
		return t, fmt.Errorf("instaclustr cluster %q has no addressable nodes yet (status %s). "+
			"Wait for the cluster to finish provisioning and re-run", clusterID, t.Status)
	}
	return t, nil
}

// EnsureInstaclustrRole makes the collector's read-only role exist WITH the
// given password, connecting as the cluster's default user (CREATEROLE +
// pg_monitor — no superuser, no support ticket). Convergent on re-runs and
// reinstalls: an existing role gets ALTER ROLE ... PASSWORD, because every
// install generates a fresh password and a skipped CREATE would strand the
// collector with a credential Postgres has never seen. Grants: pg_monitor
// for the stats views, and pg_read_all_data so schema capture (pg_dump) can
// SELECT what it dumps — the latter tolerated as missing on pre-PG14
// servers, where the role and the monitor grant still land.
//
// Error messages never include statement text: the CREATE/ALTER statements
// carry the live password, and these errors reach terminals and CI logs.
func EnsureInstaclustrRole(ctx context.Context, dsn, user, password string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("%w to create the monitoring role: %w", errClusterUnreachable, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// The role name is quoted as an identifier: every caller passes the
	// constant today, but this signature accepts any string.
	role := pgx.Identifier{user}.Sanitize()
	quoted := strings.ReplaceAll(password, "'", "''")
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, quoted)); err != nil {
		if !isBenignGrantErr(err) {
			return fmt.Errorf("creating role %s failed: %w", user, redactPassword(err, quoted, password))
		}
		if _, aerr := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s WITH LOGIN PASSWORD '%s'", role, quoted)); aerr != nil {
			return fmt.Errorf("updating role %s's password failed: %w", user, redactPassword(aerr, quoted, password))
		}
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT pg_monitor TO %s", role)); err != nil && !isBenignGrantErr(err) {
		return fmt.Errorf("granting pg_monitor to %s failed: %w", user, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT pg_read_all_data TO %s", role)); err != nil && !isBenignGrantErr(err) {
		// 42704 undefined_object: the role does not exist before PG 14 — the
		// monitor grant above still stands, so degrade rather than fail.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42704" {
			return fmt.Errorf("granting pg_read_all_data to %s failed: %w", user, err)
		}
	}
	return nil
}

// redactPassword scrubs the role password (raw and SQL-quoted forms) from an
// error before it can reach a terminal — some server errors echo statement
// fragments.
func redactPassword(err error, forms ...string) error {
	msg := err.Error()
	for _, f := range forms {
		if f != "" {
			msg = strings.ReplaceAll(msg, f, "[redacted]")
		}
	}
	return errors.New(msg)
}

// errClusterUnreachable tags a connection-establishment failure from
// EnsureInstaclustrRole, so RetriableRoleError recognizes it structurally
// rather than by matching the wrapper's prose.
var errClusterUnreachable = errors.New("cannot connect to the cluster")

// RetriableRoleError reports whether a role-ensure failure is worth another
// attempt: only connection-establishment failures are — a freshly created
// firewall rule takes a moment to pass packets, and the symptom is a dial
// timeout or refusal. SQL-level failures are deterministic; retrying them
// just multiplies the wait before the user sees the real error.
func RetriableRoleError(err error) bool {
	if !errors.Is(err, errClusterUnreachable) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "connection refused") || strings.Contains(msg, "i/o") ||
		strings.Contains(msg, "unreachable") || strings.Contains(msg, "reset")
}

// GenerateInstaclustrPassword returns a random password safe to embed in a
// single-quoted SQL literal and an env-file line (alphanumeric only).
func GenerateInstaclustrPassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 28)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// BuildInstaclustrComponent renders the [component] block for an Instaclustr
// cluster. The api_key is an env reference — the literal key never enters
// the config file, matching how the database password is handled.
// passwordEnv names the password variable of the deploy substrate: the docker
// env-file uses COLLECTOR_DB_PASSWORD; the Fargate task definition names
// DBG_DB_PASSWORD (fed from Secrets Manager).
func BuildInstaclustrComponent(t InstaclustrTarget, seedHost string, port int, databases []string, sslMode, caCert, apiUsername string, usePrivate bool, passwordEnv string) Component {
	if sslMode == "" {
		// `require` (encrypt, no verify): every Instaclustr node negotiates
		// TLS, but its certificate chains to a per-cluster CA that is only
		// distributed for encryption-enabled clusters — and this CLI's docker
		// CA mount replaces the container's system trust store, which the
		// collector's own control-plane TLS needs. verify-full support waits
		// on a CA-bundling story that keeps both trust roots.
		sslMode = "require"
	}
	return Component{
		Name:   t.Name,
		Engine: "postgres",
		Provider: Provider{
			Type:                "instaclustr",
			ClusterID:           t.ClusterID,
			CloudProvider:       t.CloudProvider,
			Region:              t.Region,
			APIUsername:         apiUsername,
			APIKey:              "${" + InstaclustrAPIKeyEnv + "}",
			UsePrivateAddresses: usePrivate,
		},
		Auth: Auth{
			Method:   "password",
			User:     InstaclustrMonitorUser,
			Password: "${" + passwordEnv + "}",
		},
		Connect: Connect{
			Host:      seedHost,
			Port:      port,
			Databases: databases,
			SSLMode:   sslMode,
			CACert:    caCert,
		},
	}
}

// BuildInstaclustr assembles the full collector config for one Instaclustr
// cluster. Kept separate from Build (the self-hosted docker path) so the two
// evolve independently.
func BuildInstaclustr(agentID, tenantID string, comp Component, eps Endpoints) Config {
	return Config{
		Dbgorilla: Dbgorilla{
			AgentID:      agentID,
			TenantID:     tenantID,
			Secret:       "${" + SecretEnv + "}",
			OpampBaseURL: eps.OpampBaseURL,
			OtlpBaseURL:  eps.OtlpBaseURL,
			AuthBaseURL:  eps.AuthBaseURL,
		},
		Component: []Component{comp},
		Topology:  Topology{Interval: "60s"},
		Commands:  Commands{Enabled: false},
	}
}

// InstaclustrAdminDSN is the connection string for the transient
// role-creation step: the cluster's default user against one node.
// sslmode=require always works — Instaclustr nodes negotiate TLS even on
// clusters provisioned without client-to-cluster encryption; connect_timeout
// keeps a firewalled node from hanging the install.
func InstaclustrAdminDSN(host string, port int, password string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("icpostgresql", password),
		Host:     fmt.Sprintf("%s:%d", host, port),
		Path:     "/postgres",
		RawQuery: "sslmode=require&connect_timeout=8",
	}
	return u.String()
}

// InstaclustrAdminDSNAs is InstaclustrAdminDSN for an arbitrary user — the
// preflight runs as the monitoring role itself, so what it verifies is what
// the collector will actually experience.
func InstaclustrAdminDSNAs(user, password, host string, port int) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     fmt.Sprintf("%s:%d", host, port),
		Path:     "/postgres",
		RawQuery: "sslmode=require&connect_timeout=8",
	}
	return u.String()
}

// AllowCIDR normalizes a user-supplied allow address into the single-host
// CIDR the firewall API wants: a bare IP gains /32 (or /128), an existing
// CIDR passes through, anything else is refused with what to pass instead.
func AllowCIDR(s string) (string, error) {
	s = strings.TrimSpace(s)
	if addr, err := netip.ParseAddr(s); err == nil {
		if addr.Is6() {
			return s + "/128", nil
		}
		return s + "/32", nil
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		// A /0 allowlists the entire internet, which defeats the firewall the
		// entry lives in — refused rather than warned, since nothing this CLI
		// sets up needs it (the console is there for a deliberate open rule).
		if p.Bits() == 0 {
			return "", fmt.Errorf("refusing to allowlist %s: it opens the cluster's firewall to the whole "+
				"internet. Pass the collector's actual egress IP, or a CIDR that covers only it", p)
		}
		return p.String(), nil
	}
	return "", fmt.Errorf("%q is not an IP address or CIDR — pass e.g. 203.0.113.10 or 203.0.113.0/24", s)
}

// PublicEgressIP asks a well-known reflector for this machine's public IP —
// the address the Instaclustr firewall must allow for a locally-run
// collector. The response is validated as an address so a captive portal's
// HTML never reaches the firewall API. Seam-shaped for tests.
var PublicEgressIP = func(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	resp, err := instaclustrClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot determine this machine's public IP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", fmt.Errorf("cannot determine this machine's public IP: %w", err)
	}
	ip := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot determine this machine's public IP (HTTP %d). "+
			"Pass --allow-ip explicitly if this machine has no direct internet path", resp.StatusCode)
	}
	if _, err := netip.ParseAddr(ip); err != nil {
		return "", fmt.Errorf("the public-IP check returned something that is not an address "+
			"(a captive portal?). Pass --allow-ip explicitly: %w", err)
	}
	return ip, nil
}
