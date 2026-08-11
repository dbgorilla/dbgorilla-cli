package cmd

import "testing"

// The docker (local) target points at a database on the developer's own
// machine, and stock Postgres ships with ssl=off -- so the flag default of
// verify-full made the documented `dbg collector install` fail on the database
// a local-dev user actually has. Local defaults to disable; AWS is untouched
// and an explicit flag always wins.
func TestLocalSSLMode(t *testing.T) {
	t.Run("docker default is disable", func(t *testing.T) {
		c := baseCmd()
		c.Flags().String("ssl-mode", "verify-full", "")
		if got := localSSLMode(c); got != "disable" {
			t.Errorf("localSSLMode = %q, want disable", got)
		}
	})

	t.Run("explicit flag wins", func(t *testing.T) {
		for _, mode := range []string{"require", "verify-ca", "verify-full"} {
			c := baseCmd()
			c.Flags().String("ssl-mode", "verify-full", "")
			mustSet(t, c, "ssl-mode", mode)
			if got := localSSLMode(c); got != mode {
				t.Errorf("localSSLMode = %q, want %q", got, mode)
			}
		}
	})
}
