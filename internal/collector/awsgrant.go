package collector

import (
	"net"
	"net/url"
	"strconv"
	"time"
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

// CommandRequest is how the caller's flags landed, decoupled from cobra: the
// command layer reads the flags, this layer applies the precedence.
type CommandRequest struct {
	// ForcedOff is an explicit hard "no query analysis" — --enable-commands=false
	// or --commands="" — for policies that forbid the collector issuing any
	// queries. It clears every database and skips the prompt.
	ForcedOff bool
	// Explicit reports that --commands was given (even empty), which applies to
	// every database and suppresses the interactive checklist.
	Explicit bool
	// Commands is the --commands value, unclamped.
	Commands []string
}

// ResolveCommands settles, per database, which query-analysis commands the
// collector may run, storing them on each target, and reports whether analysis
// is on at all. That gate is implicit: on iff at least one database ended up
// with a command. There is no separate yes/no switch — the per-database choice
// is the switch.
//
// Precedence per component: commands from --config win; else an explicit
// --commands applies to all; else prompt, if the caller supplied one; else every
// command the engine supports. All are engine-clamped.
//
// prompt is the interactive per-database checklist. nil means non-interactive,
// which takes the full-catalog default — keeping the terminal handling in the
// command layer and this precedence testable on its own.
func ResolveCommands(targets []AwsTarget, req CommandRequest, prompt func(AwsTarget) []string) bool {
	if req.ForcedOff {
		for i := range targets {
			targets[i].Commands = nil
		}
		return false
	}
	enabled := false
	for i := range targets {
		switch {
		case len(targets[i].Commands) > 0: // from --config: keep, clamped
			targets[i].Commands = AwsCommandsFor(targets[i].Commands)
		case req.Explicit:
			targets[i].Commands = AwsCommandsFor(req.Commands)
		case prompt != nil:
			targets[i].Commands = prompt(targets[i])
		default:
			targets[i].Commands = AwsCommandsFor(nil)
		}
		if len(targets[i].Commands) > 0 {
			enabled = true // implicit gate: any database with a command turns it on
		}
	}
	return enabled
}
