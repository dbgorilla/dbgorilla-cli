package collector

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The policy behind the collector's database grant: which databases need one,
// whether they can be reached from where the CLI runs, and what admin DSN to
// use. The command layer owns the prompting and printing; everything decided
// here is testable without a cobra.Command.

// defaultPort is the port to assume when a target carries none. The aws target
// is PostgreSQL-only (see ErrUnsupportedEngine), so this is unambiguous.
const defaultPort = 5432

// reachableTimeout bounds the TCP probe. It only decides whether to offer to run
// the grant, so a slow answer is worse than a wrong one — the fallback (print
// the SQL) is always available.
const reachableTimeout = 3 * time.Second

// GrantPlan is what the CLI should do about the collector's DB grant.
type GrantPlan struct {
	// Targets are the databases needing a grant: one per database server, IAM
	// auth only. Empty means there is nothing to grant.
	Targets []AwsTarget
	// Unreachable holds the instance ids of Targets whose endpoint could not be
	// reached from this host. A private RDS usually can't be, and offering to run
	// a grant that can only fail is worse than printing the SQL.
	Unreachable []string
}

// PlanGrants decides which databases need the collector's grant and whether they
// can be reached from here. probe is the reachability check; nil skips it (the
// caller already knows it wants to run, or only needs the target list).
func PlanGrants(targets []AwsTarget, probe func(addr string) error) GrantPlan {
	// Password-auth databases use the customer's own credentialed user, so the
	// IAM (rds_iam) grant does not apply to them.
	var iam []AwsTarget
	for _, t := range targets {
		if t.AuthMethod != "password" {
			iam = append(iam, t)
		}
	}
	plan := GrantPlan{Targets: DedupeTargetsByInstance(iam)}
	if probe == nil {
		return plan
	}
	for _, t := range plan.Targets {
		if err := probe(TargetDial(t)); err != nil {
			plan.Unreachable = append(plan.Unreachable, t.InstanceID)
		}
	}
	return plan
}

// Reachable reports whether a host:port accepts a TCP connection right now.
// The default probe for PlanGrants.
func Reachable(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, reachableTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// TargetDial is the host:port to reach a target's database server on.
func TargetDial(t AwsTarget) string {
	port := t.Port
	if port == 0 {
		port = defaultPort
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// DedupeTargetsByInstance keeps one target per database server. The grant is
// per-server, and a multi-database config can name the same server more than
// once.
func DedupeTargetsByInstance(targets []AwsTarget) []AwsTarget {
	seen := map[string]bool{}
	var out []AwsTarget
	for _, t := range targets {
		if seen[t.InstanceID] {
			continue
		}
		seen[t.InstanceID] = true
		out = append(out, t)
	}
	return out
}

// AdminDSN builds a libpq URL for the admin grant connection to a target's
// database server (password auth; the grants run from the default database).
func AdminDSN(user, password string, t AwsTarget) string {
	// This connection carries an admin credential, so an unset ssl_mode must not
	// silently become "require" — that encrypts without validating the server,
	// which is exactly the case a MITM needs. Match what discovery already
	// assigns every target (mergeInstance/mergeCluster default to verify-full);
	// if verification fails, the caller degrades to printing the SQL, so failing
	// closed here costs the operator a copy-paste, not the install.
	ssl := t.SSLMode
	if ssl == "" {
		ssl = "verify-full"
	}
	q := url.Values{}
	q.Set("sslmode", ssl)
	// Fail fast when the database isn't reachable from here (a private RDS often
	// isn't) so the grant falls back to printing the SQL instead of hanging.
	q.Set("connect_timeout", "8")
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     TargetDial(t),
		Path:     "/postgres",
		RawQuery: q.Encode(),
	}).String()
}

// GrantFailure says why running the grant failed, so the CLI can name the fix
// instead of guessing. Each kind has a different remedy: a wrong admin login
// is fixed by re-running with the right one, an unreachable database by
// running the SQL somewhere that can reach it.
type GrantFailure int

const (
	// GrantFailedOther is anything not classified below; the error itself is shown.
	GrantFailedOther GrantFailure = iota
	// GrantFailedAuth: the server rejected the admin login (wrong password, or
	// a user that doesn't exist — Postgres reports both the same way).
	GrantFailedAuth
	// GrantFailedUnreachable: no connection from here (refused, timed out,
	// unresolvable). Typical of a private database.
	GrantFailedUnreachable
	// GrantFailedTLS: connected, but the server's certificate didn't verify.
	GrantFailedTLS
	// GrantFailedPrivilege: logged in, but that user may not create roles or
	// grant them.
	GrantFailedPrivilege
)

// ClassifyGrantError sorts a RunGrant error into a GrantFailure.
func ClassifyGrantError(err error) GrantFailure {
	if err == nil {
		return GrantFailedOther
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		// 28P01 invalid_password; 28000 invalid_authorization_specification
		// (no such role, or no pg_hba entry for it).
		case "28P01", "28000":
			return GrantFailedAuth
		// 42501 insufficient_privilege.
		case "42501":
			return GrantFailedPrivilege
		}
		return GrantFailedOther
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &verify) {
		return GrantFailedTLS
	}
	var netErr net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	if errors.As(err, &dnsErr) || errors.As(err, &opErr) || errors.As(err, &netErr) {
		return GrantFailedUnreachable
	}
	// Fall back to the message for errors that arrive without their type
	// (flattened by a wrapper upstream).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SQLSTATE 28P01"), strings.Contains(msg, "SQLSTATE 28000"):
		return GrantFailedAuth
	case strings.Contains(msg, "SQLSTATE 42501"):
		return GrantFailedPrivilege
	case strings.Contains(msg, "x509:"), strings.Contains(msg, "tls: failed to verify"):
		return GrantFailedTLS
	case strings.Contains(msg, "dial "), strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"):
		return GrantFailedUnreachable
	}
	return GrantFailedOther
}
