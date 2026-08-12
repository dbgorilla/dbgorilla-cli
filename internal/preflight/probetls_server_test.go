package preflight

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"math/big"
	"net"
	"testing"
	"time"
)

// A server that DOES speak TLS must never be reported as unsupported — that is
// the verdict which authorizes sending a database password in clear text. The
// probe deliberately uses sslmode=require rather than verify-full, so a server
// behind a certificate this machine does not trust still answers "supported":
// its certificate is a separate problem with a separate fix. Proving that takes
// a server that actually completes a TLS handshake with an untrusted, self-
// signed certificate, which is what this builds.

// selfSignedCert mints a throwaway certificate for the fake server. It is
// deliberately not trusted by anything.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-postgres"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// errorResponse builds a Postgres ErrorResponse message.
func errorResponse(severity, code, message string) []byte {
	var body []byte
	add := func(field byte, v string) {
		body = append(body, field)
		body = append(body, v...)
		body = append(body, 0)
	}
	add('S', severity)
	add('C', code)
	add('M', message)
	body = append(body, 0) // terminator

	out := []byte{'E'}
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(body)+4))
	out = append(out, length...)
	return append(out, body...)
}

// tlsPostgres answers the SSLRequest with 'S', completes a TLS handshake with
// an untrusted certificate, then rejects the login. Returns host and port.
func tlsPostgres(t *testing.T, reply []byte) (string, int) {
	t.Helper()
	cert := selfSignedCert(t)
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
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				// SSLRequest is 8 bytes.
				if _, err := conn.Read(make([]byte, 8)); err != nil {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				defer func() { _ = tlsConn.Close() }()
				// Read the startup packet, then answer.
				_, _ = tlsConn.Read(make([]byte, 1024))
				_, _ = tlsConn.Write(reply)
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// An untrusted certificate is still TLS. Reporting this as unsupported would
// let the CLI offer to drop TLS on a server that has it.
func TestProbeTLS_UntrustedCertificateStillCountsAsSupported(t *testing.T) {
	host, port := tlsPostgres(t, errorResponse("FATAL", "28P01", `password authentication failed for user "ro"`))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := ProbeTLS(ctx, probeDSN(host, port))
	if got == TLSUnsupported {
		t.Fatal("a server behind an untrusted certificate must never be reported as having no TLS")
	}
	if got != TLSSupported {
		t.Errorf("ProbeTLS = %v, want TLSSupported (the handshake completed)", got)
	}
}

// A rejected login also proves the transport worked.
func TestProbeTLS_AuthFailureCountsAsSupported(t *testing.T) {
	host, port := tlsPostgres(t, errorResponse("FATAL", "28000",
		`no pg_hba.conf entry for host "127.0.0.1", user "ro", database "appdb", SSL on`))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if got := ProbeTLS(ctx, probeDSN(host, port)); got != TLSSupported {
		t.Errorf("ProbeTLS = %v, want TLSSupported", got)
	}
}

// An error that is neither a TLS refusal nor an auth failure is undeterminable,
// and must not authorize anything.
func TestProbeTLS_UnrelatedServerErrorIsUnknown(t *testing.T) {
	host, port := tlsPostgres(t, errorResponse("FATAL", "3D000", `database "appdb" does not exist`))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := ProbeTLS(ctx, probeDSN(host, port))
	if got == TLSUnsupported {
		t.Fatal("an unrelated failure must not be read as 'no TLS'")
	}
	if got != TLSUnknown {
		t.Errorf("ProbeTLS = %v, want TLSUnknown", got)
	}
}
