package preflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// The TLS capability probe decides whether the CLI may drop TLS automatically.
// Getting "unsupported" wrong in the permissive direction means a database
// password crosses a network in clear text, so every branch is pinned here.
//
// The fixtures are a socket that speaks the first byte of the Postgres startup
// handshake and nothing more — enough for a real pgx.Connect to reach a verdict.

// fakePostgres accepts one connection, answers the SSLRequest with reply, and
// closes. Returns host and port.
func fakePostgres(t *testing.T, reply byte) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// The SSLRequest packet is 8 bytes; read it, then answer.
				buf := make([]byte, 8)
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := conn.Read(buf); err != nil {
					return
				}
				_, _ = conn.Write([]byte{reply})
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func probeDSN(host string, port int) string {
	return ProbeTLSDSN(fmt.Sprintf("postgres://ro:pw@%s:%d/appdb?sslmode=verify-full", host, port))
}

// A stock local Postgres ships with ssl=off and answers 'N'. That is the case
// the whole feature exists for.
func TestProbeTLS_ServerRefusesTLS(t *testing.T) {
	host, port := fakePostgres(t, 'N')
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if got := ProbeTLS(ctx, probeDSN(host, port)); got != TLSUnsupported {
		t.Fatalf("ProbeTLS = %v, want TLSUnsupported", got)
	}
}

// Anything the probe cannot interpret must come back Unknown, never
// Unsupported: Unsupported is what authorizes dropping TLS.
func TestProbeTLS_UnreachableIsUnknownNotUnsupported(t *testing.T) {
	// Bind and immediately release a port so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := ProbeTLS(ctx, probeDSN(addr.IP.String(), addr.Port))
	if got == TLSUnsupported {
		t.Fatal("an unreachable server must never authorize dropping TLS")
	}
	if got != TLSUnknown {
		t.Errorf("ProbeTLS = %v, want TLSUnknown", got)
	}
}

// A server that hangs up mid-handshake is also undeterminable.
func TestProbeTLS_ImmediateCloseIsUnknown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addr := ln.Addr().(*net.TCPAddr)
	if got := ProbeTLS(ctx, probeDSN(addr.IP.String(), addr.Port)); got == TLSUnsupported {
		t.Fatal("a dropped connection must not authorize dropping TLS")
	}
}

func TestProbeTLSDSN_ForcesRequire(t *testing.T) {
	// require, not verify-full: the probe asks whether a handshake is possible
	// at all, so a server behind a private CA still answers "supported" — its
	// certificate is a separate problem with a separate fix.
	got := ProbeTLSDSN("postgres://ro:pw@db.example.com:5432/appdb?sslmode=verify-full")
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("DSN = %q, want sslmode=require", got)
	}
	if strings.Contains(got, "verify-full") {
		t.Errorf("DSN = %q, the original mode should be replaced", got)
	}

	// A DSN with no sslmode at all gets one.
	got = ProbeTLSDSN("postgres://ro:pw@db.example.com:5432/appdb")
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("DSN = %q, want sslmode=require added", got)
	}

	// Other parameters survive.
	got = ProbeTLSDSN("postgres://ro:pw@db.example.com:5432/appdb?sslmode=disable&connect_timeout=5")
	if !strings.Contains(got, "connect_timeout=5") {
		t.Errorf("DSN = %q, other parameters must be preserved", got)
	}

	// An unparseable DSN comes back untouched rather than mangled.
	if got := ProbeTLSDSN("::not a url::"); got != "::not a url::" {
		t.Errorf("DSN = %q, want the input returned unchanged", got)
	}
}

func TestIsTLSUnsupported(t *testing.T) {
	// The two phrasings a Postgres server's refusal arrives as.
	for _, msg := range []string{
		"failed to connect to `host=localhost`: server does not support SSL, but SSL was required",
		"server refused TLS connection",
		"SERVER DOES NOT SUPPORT SSL", // matching is case-insensitive
	} {
		if !IsTLSUnsupported(errors.New(msg)) {
			t.Errorf("should be recognised as a TLS refusal: %s", msg)
		}
	}

	// Everything else must not be, or the CLI would drop TLS on an unrelated
	// failure.
	for _, msg := range []string{
		"connection refused",
		"password authentication failed for user \"ro\"",
		"x509: certificate signed by unknown authority",
		"context deadline exceeded",
	} {
		if IsTLSUnsupported(errors.New(msg)) {
			t.Errorf("must NOT be read as a TLS refusal: %s", msg)
		}
	}

	if IsTLSUnsupported(nil) {
		t.Error("nil is not a TLS refusal")
	}
}

// An authentication failure proves the transport worked: the handshake got far
// enough for the server to reject the credentials. Reading it as "no TLS" would
// downgrade a perfectly good TLS connection.
func TestIsAuthFailure(t *testing.T) {
	for _, msg := range []string{
		`password authentication failed for user "ro"`,
		"authentication failed",
		`role "ro" does not exist`,
		`no pg_hba.conf entry for host "10.0.0.1", user "ro", database "appdb", SSL on`,
	} {
		if !isAuthFailure(errors.New(msg)) {
			t.Errorf("should count as a completed transport: %s", msg)
		}
	}

	for _, msg := range []string{
		"connection refused",
		"server does not support SSL",
		"context deadline exceeded",
	} {
		if isAuthFailure(errors.New(msg)) {
			t.Errorf("must NOT count as an auth failure: %s", msg)
		}
	}
}

// The remediation for a failed connection has to point the right way. Telling a
// local-dev user with no TLS to add more TLS sends them in a circle.
func TestTLSAwareConnectFix_EveryDirection(t *testing.T) {
	joined := func(err error) string { return strings.Join(tlsAwareConnectFix(err), " ") }

	noTLS := joined(errors.New("server does not support SSL, but SSL was required"))
	if !strings.Contains(noTLS, "--ssl-mode disable") {
		t.Errorf("a server without TLS must be told to disable, got: %s", noTLS)
	}

	// A private CA needs the opposite advice: keep verifying, supply the CA.
	refused := joined(errors.New("server refused TLS connection"))
	if !strings.Contains(refused, "--ca-cert") {
		t.Errorf("a TLS negotiation failure should mention the CA option, got: %s", refused)
	}
	if !strings.Contains(refused, "disable") {
		t.Errorf("it should also cover the no-TLS case, got: %s", refused)
	}

	tlsErr := joined(errors.New("tls error: handshake failure"))
	if !strings.Contains(tlsErr, "--ca-cert") {
		t.Errorf("a tls error should get the negotiation advice, got: %s", tlsErr)
	}

	generic := joined(errors.New("connection refused"))
	if !strings.Contains(generic, "verify-full") || !strings.Contains(generic, "disable") {
		t.Errorf("generic advice should name every mode, got: %s", generic)
	}
	// Every branch keeps the basics.
	for _, fix := range []string{noTLS, refused, generic} {
		if !strings.Contains(fix, "host/port") {
			t.Errorf("every remediation should still cover host/port, got: %s", fix)
		}
	}
}

// Run's own connect-failure path: it must return a single failed connect result
// carrying the direction-aware remediation, not a partial report that looks
// like the other checks passed.
func TestRun_ConnectFailureIsASingleActionableResult(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	report := Run(ctx, fmt.Sprintf("postgres://ro:pw@%s:%d/appdb?sslmode=disable", addr.IP, addr.Port))
	if len(report.Results) != 1 {
		t.Fatalf("want exactly one result, got %d: %+v", len(report.Results), report.Results)
	}
	r := report.Results[0]
	if r.Name != "connect" || r.Severity != Fail {
		t.Errorf("got %+v, want a failed connect check", r)
	}
	if len(r.Fix) == 0 {
		t.Error("a failed connection must carry remediation")
	}
	if !report.Failed() {
		t.Error("the report must read as failed")
	}
}

// Status must not hard-error on a database that is briefly unreachable, so the
// workload probe degrades to a warning.
func TestCheckWorkload_UnreachableDegradesToWarn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := CheckWorkload(ctx, fmt.Sprintf("postgres://ro:pw@%s:%d/appdb?sslmode=disable", addr.IP, addr.Port))
	if res.Severity != Warn {
		t.Errorf("severity = %v, want Warn", res.Severity)
	}
	if res.Name != "workload" {
		t.Errorf("name = %q, want workload", res.Name)
	}
}
