package collector

import (
	"crypto/x509"
	_ "embed"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Amazon RDS server certificates chain to Amazon's own RDS certificate
// authorities, which are not in operating-system trust stores. A verify-full
// connection to RDS or Aurora from a workstation therefore fails with
// "certificate signed by unknown authority" unless those CAs are supplied.
//
// certs/rds-global-bundle.pem is AWS's published bundle of every commercial
// region's RDS root CAs, unmodified, from
// https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
// (see the RDS user guide, "Using SSL/TLS to encrypt a connection to a DB
// instance"). The roots it contains are long-lived; when AWS adds a region or
// rotates a CA, refresh the file from that URL.

//go:embed certs/rds-global-bundle.pem
var rdsCABundle []byte

var (
	rdsRootsOnce sync.Once
	rdsRoots     *x509.CertPool
)

// rdsRootCAs returns the system roots plus the RDS CAs. System roots stay in
// because not every endpoint the CLI is pointed at is an RDS one (an RDS Proxy
// endpoint, for instance, presents a publicly trusted certificate).
func rdsRootCAs() *x509.CertPool {
	rdsRootsOnce.Do(func() {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(rdsCABundle)
		rdsRoots = pool
	})
	return rdsRoots
}

// trustRDSCAs makes a verifying connection accept RDS server certificates. It
// only fills in roots that were left unset: an explicit sslrootcert (or a
// ~/.postgresql/root.crt picked up the libpq way) is the operator's choice and
// is kept. The tls.Config is updated in place rather than replaced, because
// pgx's verify-ca check reads RootCAs from that same config at handshake time.
func trustRDSCAs(cfg *pgx.ConnConfig) {
	if cfg.TLSConfig != nil && cfg.TLSConfig.RootCAs == nil {
		cfg.TLSConfig.RootCAs = rdsRootCAs()
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig != nil && fb.TLSConfig.RootCAs == nil {
			fb.TLSConfig.RootCAs = rdsRootCAs()
		}
	}
}
