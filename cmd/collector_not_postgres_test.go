package cmd

import (
	"net"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A server that drops the connection on Postgres's SSLRequest, as Cassandra
// does, stops the install with a message that names the route that works.
func TestRequirePostgresRefusesOtherServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			buf := make([]byte, 8)
			_, _ = c.Read(buf)
			_ = c.Close()
		}
	}()
	err = requirePostgres(ln.Addr().String())
	if err == nil {
		t.Fatal("want an error for a non-Postgres server")
	}
	for _, want := range []string{"is not a PostgreSQL server", "Nothing has been provisioned", "Create credentials", "https://www.dbgorilla.com/docs/connect/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
}

func TestRequirePostgresAcceptsPostgres(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			buf := make([]byte, 8)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("N"))
			_ = c.Close()
		}
	}()
	if err := requirePostgres(ln.Addr().String()); err != nil {
		t.Fatalf("Postgres refused: %v", err)
	}
}

// listenAnswering serves one connection: it reads the SSLRequest and writes
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

func yesCmd(yes bool) *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("yes", yes, "")
	return c
}

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

func TestCheckTargetIsPostgres(t *testing.T) {
	if err := checkTargetIsPostgres(yesCmd(false), listenAnswering(t, "N")); err != nil {
		t.Errorf("PostgreSQL refused: %v", err)
	}
	err := checkTargetIsPostgres(yesCmd(true), listenAnswering(t, ""))
	if err == nil || !strings.Contains(err.Error(), "is not a PostgreSQL server") {
		t.Errorf("non-PostgreSQL server: got %v", err)
	}
	if err := checkTargetIsPostgres(yesCmd(true), closedAddr(t)); err != nil {
		t.Errorf("unreachable with --yes should continue, got %v", err)
	}
	if err := checkTargetIsPostgres(yesCmd(false), closedAddr(t)); err == nil || err.Error() != "aborted" {
		t.Errorf("unreachable without --yes should abort, got %v", err)
	}
}
