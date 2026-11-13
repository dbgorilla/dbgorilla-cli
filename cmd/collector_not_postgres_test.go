package cmd

import (
	"net"
	"strings"
	"testing"
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
