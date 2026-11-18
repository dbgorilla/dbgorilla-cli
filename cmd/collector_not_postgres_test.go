package cmd

import (
	"net"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// listenAnswering serves connections: each reads the SSLRequest and writes
// reply, or closes without a word when reply is empty.
func listenAnswering(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 8)
			_, _ = c.Read(buf)
			if reply != "" {
				_, _ = c.Write([]byte(reply))
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// installFlags builds a command carrying the install's --yes and --force.
func installFlags(yes, force bool) *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("yes", yes, "")
	c.Flags().Bool("force", force, "")
	return c
}

func yesCmd(yes bool) *cobra.Command { return installFlags(yes, false) }

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// A server that replies with bytes PostgreSQL never sends stops the install
// with a message that names the route that works.
func TestRequirePostgresRefusesForeignReply(t *testing.T) {
	err := requirePostgres(installFlags(true, false), listenAnswering(t, "HTTP/1.1 400 Bad Request\r\n"))
	if err == nil {
		t.Fatal("want an error for a non-Postgres server")
	}
	for _, want := range []string{"is not a PostgreSQL server", "Nothing has been provisioned", "Create credentials", "https://www.dbgorilla.com/docs/connect/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
}

// --force carries on past a definite foreign reply, as it does past other
// preflight failures.
func TestRequirePostgresForceBypassesForeignReply(t *testing.T) {
	if err := requirePostgres(installFlags(false, true), listenAnswering(t, "HTTP/1.1 400")); err != nil {
		t.Fatalf("--force should carry on, got %v", err)
	}
}

func TestRequirePostgresAcceptsPostgres(t *testing.T) {
	for _, reply := range []string{"N", "S"} {
		if err := requirePostgres(installFlags(false, false), listenAnswering(t, reply)); err != nil {
			t.Fatalf("Postgres (%q) refused: %v", reply, err)
		}
	}
}

// A connection closed without a reply is what Cassandra 5.0.9 does, but also
// what a forwarder with no backend does (docker-proxy while the container
// starts, ssh -L, kubectl port-forward, Cloud SQL Auth Proxy). It must not be
// a hard stop: it warns and asks, and --yes or --force carries on.
func TestRequirePostgresClosedIsNotAHardStop(t *testing.T) {
	forwarder := listenAnswering(t, "")
	if err := requirePostgres(installFlags(true, false), forwarder); err != nil {
		t.Errorf("closed with --yes should continue, got %v", err)
	}
	if err := requirePostgres(installFlags(false, true), forwarder); err != nil {
		t.Errorf("closed with --force should continue, got %v", err)
	}
	// No --yes and no terminal: stdin is empty, so the prompt declines.
	if err := requirePostgres(installFlags(false, false), forwarder); err == nil || err.Error() != "aborted" {
		t.Errorf("closed without --yes should abort at the prompt, got %v", err)
	}
}

func TestWarnClosedOnProbeNamesBothCauses(t *testing.T) {
	msg := warnClosedOnProbe("127.0.0.1:9042")
	for _, want := range []string{"not a PostgreSQL server", "proxy or port-forward", "could not reach the database", "Create credentials"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning lacks %q:\n%s", want, msg)
		}
	}
}

func TestCheckTargetIsPostgres(t *testing.T) {
	if err := checkTargetIsPostgres(yesCmd(false), listenAnswering(t, "N")); err != nil {
		t.Errorf("PostgreSQL refused: %v", err)
	}
	err := checkTargetIsPostgres(yesCmd(true), listenAnswering(t, "HTTP/1.1 400"))
	if err == nil || !strings.Contains(err.Error(), "is not a PostgreSQL server") {
		t.Errorf("foreign reply even with --yes: got %v", err)
	}
	if err := checkTargetIsPostgres(yesCmd(true), listenAnswering(t, "")); err != nil {
		t.Errorf("closed without reply, with --yes, should continue: got %v", err)
	}
	if err := checkTargetIsPostgres(yesCmd(true), closedAddr(t)); err != nil {
		t.Errorf("unreachable with --yes should continue, got %v", err)
	}
	if err := checkTargetIsPostgres(yesCmd(false), closedAddr(t)); err == nil || err.Error() != "aborted" {
		t.Errorf("unreachable without --yes should abort, got %v", err)
	}
}
