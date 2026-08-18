package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/preflight"
)

// setInstallStubs replaces the Docker/DB install seams so runInstall can be
// driven without a real engine or database.
func setInstallStubs(t *testing.T, dockerErr error, rep preflight.Report, runErr error) {
	t.Helper()
	origD, origP, origR, origI := dockerAvailable, runPreflight, runContainer, pinImage
	dockerAvailable = func() error { return dockerErr }
	runPreflight = func(context.Context, string) preflight.Report { return rep }
	runContainer = func(collector.Runner) error { return runErr }
	// The default image is a moving tag, so resolving it to a digest now shells
	// out to `docker pull`. isolate() empties PATH on purpose, so stub it.
	pinImage = func(ref string) (string, error) { return ref + "@sha256:testdigest", nil }
	t.Cleanup(func() {
		dockerAvailable, runPreflight, runContainer, pinImage = origD, origP, origR, origI
	})
}

func cleanReport() preflight.Report {
	return preflight.Report{Results: []preflight.Result{{Name: "connect", Severity: preflight.OK, Detail: "ok"}}}
}

func failReport() preflight.Report {
	return preflight.Report{Results: []preflight.Result{{Name: "server version", Severity: preflight.Fail, Detail: "too old"}}}
}

// installServer serves the collector provisioning + status API for one agent.
func installServer(t *testing.T, agentID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0_2/collectors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"agent_id":"`+agentID+`","secret":"sek","tenant_id":"ten","domain":"dep.example"}`)
			return
		}
		w.WriteHeader(http.StatusOK) // CollectorSupported probe
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/v0_2/collectors/"+agentID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent) // DELETE (rollback / uninstall)
	})
	mux.HandleFunc("/api/v0_2/collectors/"+agentID+"/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"connected"}`)
	})
	return httptest.NewServer(mux)
}

// openDBPort returns a real listening loopback addr so checkReachable succeeds
// without a database behind it (the deep DB preflight is stubbed separately).
func openDBPort(t *testing.T) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

func TestRunInstall_HappyPath(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, nil, cleanReport(), nil)
	host, port := openDBPort(t)
	srv := installServer(t, "agent-x")
	defer srv.Close()

	c := installTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", host)
	_ = c.Flags().Set("db-port", port)
	_ = c.Flags().Set("db-password", "pw")

	out := capture(t, func() {
		if err := runInstall(c, nil); err != nil {
			t.Fatalf("install err: %v", err)
		}
	})
	for _, want := range []string{
		"Database reachable", "Collector provisioned (agent agent-x",
		"Wrote config", "Container started", "Collector connected", "Collector installed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
	st, _ := collector.LoadState()
	if st == nil || st.AgentID != "agent-x" {
		t.Errorf("state not persisted: %+v", st)
	}
}

func TestRunInstall_ContainerStartRollsBack(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, nil, cleanReport(), errors.New("docker run boom"))
	host, port := openDBPort(t)
	srv := installServer(t, "agent-x")
	defer srv.Close()

	c := installTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", host)
	_ = c.Flags().Set("db-port", port)
	_ = c.Flags().Set("db-password", "pw")

	var err error
	out := capture(t, func() { err = runInstall(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "Rolled back") {
		t.Fatalf("err=%v, want rollback", err)
	}
	if !strings.Contains(out, "rolling back the provisioned identity") {
		t.Errorf("rollback not surfaced:\n%s", out)
	}
	// Rollback must not leave persisted state behind.
	if st, _ := collector.LoadState(); st != nil {
		t.Errorf("state should not persist after a failed start: %+v", st)
	}
}

func TestRunInstall_PreflightFailure(t *testing.T) {
	t.Run("without --force aborts", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		setInstallStubs(t, nil, failReport(), nil)
		host, port := openDBPort(t)
		srv := installServer(t, "agent-x")
		defer srv.Close()
		c := installTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("db-user", "ro")
		_ = c.Flags().Set("db-host", host)
		_ = c.Flags().Set("db-port", port)
		_ = c.Flags().Set("db-password", "pw")
		var err error
		_ = capture(t, func() { err = runInstall(c, nil) })
		if err == nil || !strings.Contains(err.Error(), "preflight failed") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("with --force continues", func(t *testing.T) {
		isolate(t)
		writeTokens(t)
		setInstallStubs(t, nil, failReport(), nil)
		host, port := openDBPort(t)
		srv := installServer(t, "agent-x")
		defer srv.Close()
		c := installTestCmd()
		mustSet(t, c, "api-url", srv.URL)
		_ = c.Flags().Set("db-user", "ro")
		_ = c.Flags().Set("db-host", host)
		_ = c.Flags().Set("db-port", port)
		_ = c.Flags().Set("db-password", "pw")
		_ = c.Flags().Set("force", "true")
		out := capture(t, func() {
			if err := runInstall(c, nil); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
		if !strings.Contains(out, "Continuing despite preflight failures") {
			t.Errorf("out=%q", out)
		}
	})
}

func TestRunInstall_DockerUnavailable(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, errors.New("docker not running"), cleanReport(), nil)
	c := installTestCmd()
	mustSet(t, c, "api-url", "https://dep.example")
	_ = c.Flags().Set("db-user", "ro")
	err := runInstall(c, nil)
	if err == nil || !strings.Contains(err.Error(), "docker not running") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunInstall_CollectorUnsupported(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, nil, cleanReport(), nil)
	srv := statusServer(t, 404, "") // v0_2 route absent -> unsupported
	defer srv.Close()
	c := installTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("db-user", "ro")
	err := runInstall(c, nil)
	if !errors.Is(err, api.ErrCollectorUnsupported) {
		t.Fatalf("err=%v, want ErrCollectorUnsupported", err)
	}
}

func TestRunInstall_UnreachableThenProvisionError(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, nil, cleanReport(), nil)
	// Provision POST fails; also exercises the unreachable-DB + --yes confirm path.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0_2/collectors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"detail":"nope"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := installTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", "127.0.0.1")
	_ = c.Flags().Set("db-port", "1") // nothing listening -> unreachable
	_ = c.Flags().Set("db-password", "pw")
	_ = c.Flags().Set("yes", "true") // auto-confirm the "continue anyway?" prompt

	var err error
	out := capture(t, func() { err = runInstall(c, nil) })
	if err == nil {
		t.Fatal("expected provisioning error")
	}
	if !strings.Contains(out, "Continue anyway") && !strings.Contains(out, "cannot reach database") {
		t.Errorf("unreachable path not exercised:\n%s", out)
	}
}

func TestRunInstall_UnreachableDeclineAborts(t *testing.T) {
	isolate(t)
	writeTokens(t)
	setInstallStubs(t, nil, cleanReport(), nil)
	srv := statusServer(t, 200, `[]`) // supported
	defer srv.Close()
	setStdin(t, "n\n") // decline the "continue anyway?" prompt

	c := installTestCmd()
	mustSet(t, c, "api-url", srv.URL)
	_ = c.Flags().Set("db-user", "ro")
	_ = c.Flags().Set("db-host", "127.0.0.1")
	_ = c.Flags().Set("db-port", "1")
	_ = c.Flags().Set("db-password", "pw")

	var err error
	_ = capture(t, func() { err = runInstall(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err=%v, want aborted", err)
	}
}
