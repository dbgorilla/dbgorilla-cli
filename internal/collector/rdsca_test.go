package collector

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The embedded bundle is what makes a workstation's verify-full connection to
// RDS succeed. It must parse, and must be the RDS roots.
func TestRDSCABundle_ParsesAndIsTheRDSRoots(t *testing.T) {
	rest := rdsCABundle
	n := 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatalf("certificate %d: %v", n, err)
		}
		if !cert.IsCA || !strings.HasPrefix(cert.Subject.CommonName, "Amazon RDS ") {
			t.Errorf("certificate %d (%q) isn't an Amazon RDS CA", n, cert.Subject.CommonName)
		}
		n++
	}
	if n < 20 {
		t.Errorf("bundle holds %d certificates; the published bundle has one set per region", n)
	}
}

func TestTrustRDSCAs(t *testing.T) {
	t.Run("a verifying connection gets the RDS roots", func(t *testing.T) {
		cfg, err := pgx.ParseConfig("postgres://u:p@db.example:5432/postgres?sslmode=verify-full&sslrootcert=")
		if err != nil {
			t.Fatal(err)
		}
		trustRDSCAs(cfg)
		if cfg.TLSConfig == nil || cfg.TLSConfig.RootCAs != rdsRootCAs() {
			t.Error("verify-full must verify against system + RDS roots")
		}
		if cfg.TLSConfig.ServerName != "db.example" {
			t.Errorf("hostname verification must stay on, ServerName = %q", cfg.TLSConfig.ServerName)
		}
	})

	t.Run("an explicit root certificate is kept", func(t *testing.T) {
		cfg, err := pgx.ParseConfig("postgres://u:p@db.example:5432/postgres?sslmode=verify-full&sslrootcert=")
		if err != nil {
			t.Fatal(err)
		}
		own := x509.NewCertPool()
		cfg.TLSConfig.RootCAs = own
		trustRDSCAs(cfg)
		if cfg.TLSConfig.RootCAs != own {
			t.Error("an operator-supplied root pool must not be replaced")
		}
	})

	t.Run("fallback TLS configs get the roots too", func(t *testing.T) {
		cfg, err := pgx.ParseConfig("postgres://u:p@db.example:5432/postgres?sslmode=prefer&sslrootcert=")
		if err != nil {
			t.Fatal(err)
		}
		trustRDSCAs(cfg)
		for _, fb := range cfg.Fallbacks {
			if fb.TLSConfig != nil && fb.TLSConfig.RootCAs == nil {
				t.Error("a TLS fallback was left on system roots only")
			}
		}
	})

	t.Run("plaintext is left alone", func(t *testing.T) {
		cfg, err := pgx.ParseConfig("postgres://u:p@db.example:5432/postgres?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		trustRDSCAs(cfg)
		if cfg.TLSConfig != nil {
			t.Error("sslmode=disable must stay plaintext")
		}
	})
}
