package preflight

import (
	"net"
	"testing"
	"time"
)

// serve accepts one connection, reads the 8-byte SSLRequest, then runs answer.
func serve(t *testing.T, answer func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 8)
		_, _ = c.Read(buf)
		answer(c)
	}()
	return ln.Addr().String()
}

func TestProbePostgres(t *testing.T) {
	cases := []struct {
		name   string
		answer func(net.Conn)
		want   Speaks
	}{
		{"postgres without TLS", func(c net.Conn) { _, _ = c.Write([]byte("N")) }, SpeaksPostgres},
		{"postgres with TLS", func(c net.Conn) { _, _ = c.Write([]byte("S")) }, SpeaksPostgres},
		{"closes the connection, as Cassandra does", func(net.Conn) {}, SpeaksOther},
		{"replies with something else", func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400")) }, SpeaksOther},
		{"says nothing", func(net.Conn) { time.Sleep(500 * time.Millisecond) }, SpeaksUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := serve(t, tc.answer)
			if got := ProbePostgres(addr, 200*time.Millisecond); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbePostgresNoListener(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	if got := ProbePostgres(addr, 200*time.Millisecond); got != SpeaksUnknown {
		t.Fatalf("got %v, want SpeaksUnknown", got)
	}
}

func TestProbePostgresOldServerError(t *testing.T) {
	addr := serve(t, func(c net.Conn) { _, _ = c.Write([]byte("E")) })
	if got := ProbePostgres(addr, 200*time.Millisecond); got != SpeaksPostgres {
		t.Fatalf("got %v, want SpeaksPostgres", got)
	}
}

func TestProbeConnWriteFails(t *testing.T) {
	ours, theirs := net.Pipe()
	_ = theirs.Close()
	defer func() { _ = ours.Close() }()
	if got := probeConn(ours, 200*time.Millisecond); got != SpeaksOther {
		t.Fatalf("got %v, want SpeaksOther", got)
	}
}
