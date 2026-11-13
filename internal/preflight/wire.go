package preflight

import (
	"errors"
	"net"
	"os"
	"time"
)

// Speaks reports whether the server at addr speaks PostgreSQL.
type Speaks int

const (
	// SpeaksUnknown means the probe could not tell (no connection, or no reply
	// in time). Carry on and let the real preflight report what is wrong.
	SpeaksUnknown Speaks = iota
	// SpeaksPostgres means the server answered the SSL request the way only a
	// PostgreSQL server does.
	SpeaksPostgres
	// SpeaksOther means something answered on the port but it is not
	// PostgreSQL: it closed or reset the connection, or replied with bytes
	// PostgreSQL never sends.
	SpeaksOther
)

// sslRequest is PostgreSQL's SSLRequest message: length 8, code 80877103.
// Every PostgreSQL server since 7.0 answers it with one byte, 'S' or 'N',
// before any authentication, so the probe needs no credentials.
var sslRequest = []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}

// ProbePostgres sends one SSLRequest to addr and reads the one-byte answer.
// It exists because the installer otherwise learns it is talking to, say,
// Cassandra only from a Postgres driver error ("unexpected EOF") that names
// the wrong cause.
func ProbePostgres(addr string, timeout time.Duration) Speaks {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return SpeaksUnknown
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(sslRequest); err != nil {
		return SpeaksOther
	}
	reply := make([]byte, 1)
	if _, err := conn.Read(reply); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return SpeaksUnknown
		}
		return SpeaksOther // closed or reset: Cassandra and most other servers do this
	}
	switch reply[0] {
	case 'S', 'N':
		return SpeaksPostgres
	case 'E':
		// A server too old to know SSLRequest replies with an error message.
		return SpeaksPostgres
	}
	return SpeaksOther
}
