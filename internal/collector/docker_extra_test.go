package collector

import (
	"errors"
	"strings"
	"testing"
)

func TestImageForVersion(t *testing.T) {
	got := ImageForVersion("0.3.1")
	want := ImageRepo + ":0.3.1"
	if got != want {
		t.Errorf("ImageForVersion = %q, want %q", got, want)
	}
}

func TestPinnedRef_resolvesDigestViaDocker(t *testing.T) {
	fakeDocker(t)
	// pull succeeds (default exit 0); inspect returns a RepoDigests entry.
	t.Setenv("HP_INSPECT_DIGEST_OUT", "dbgorillapublic.azurecr.io/dbg-collector@sha256:abc123")

	got, err := PinnedRef("dbgorillapublic.azurecr.io/dbg-collector:0.2.0")
	if err != nil {
		t.Fatalf("PinnedRef: %v", err)
	}
	want := "dbgorillapublic.azurecr.io/dbg-collector:0.2.0@sha256:abc123"
	if got != want {
		t.Errorf("PinnedRef = %q, want %q", got, want)
	}
}

func TestPinnedRef_pullFails(t *testing.T) {
	fakeDocker(t)
	t.Setenv("HP_PULL_EXIT", "1")
	t.Setenv("HP_PULL_STDERR", "Error response from daemon: manifest unknown")

	if _, err := PinnedRef("dbgorillapublic.azurecr.io/dbg-collector:9.9.9"); err == nil {
		t.Fatal("expected error when docker pull fails")
	}
}

func TestPinnedRef_inspectFails(t *testing.T) {
	fakeDocker(t)
	// pull succeeds, but the digest inspect exits non-zero.
	t.Setenv("HP_INSPECT_DIGEST_EXIT", "1")

	_, err := PinnedRef("dbgorillapublic.azurecr.io/dbg-collector:0.2.0")
	if err == nil {
		t.Fatal("expected error when digest inspect fails")
	}
	if !strings.Contains(err.Error(), "cannot resolve image digest") {
		t.Errorf("error = %v, want it to mention resolving the digest", err)
	}
}

func TestPinnedRef_malformedDigest(t *testing.T) {
	fakeDocker(t)
	// pull succeeds; inspect returns something without an @sha256 part.
	t.Setenv("HP_INSPECT_DIGEST_OUT", "no-digest-here")

	if _, err := PinnedRef("dbgorillapublic.azurecr.io/dbg-collector:0.2.0"); err == nil {
		t.Fatal("expected error for a malformed repo digest")
	}
}

func TestDockerAvailable_notOnPath(t *testing.T) {
	fakeLookPath(t, "", errors.New("not found"))
	err := DockerAvailable()
	if err == nil || !strings.Contains(err.Error(), "docker not found") {
		t.Fatalf("expected docker-not-found error, got %v", err)
	}
}

func TestDockerAvailable_engineNotResponding(t *testing.T) {
	fakeLookPath(t, "/usr/bin/docker", nil)
	fakeDocker(t)
	t.Setenv("HP_INFO_EXIT", "1")
	t.Setenv("HP_INFO_STDERR", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock")

	err := DockerAvailable()
	if err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("expected engine-not-responding error, got %v", err)
	}
}

func TestDockerAvailable_ok(t *testing.T) {
	fakeLookPath(t, "/usr/bin/docker", nil)
	fakeDocker(t)
	// HP_INFO_EXIT unset -> info exits 0.
	if err := DockerAvailable(); err != nil {
		t.Fatalf("expected docker available, got %v", err)
	}
}

func newRunner() Runner {
	return Runner{
		Name:        DefaultContainerName,
		Image:       DefaultImage,
		ConfigPath:  "/home/dev/.config/dbgorilla/collector/collector.toml",
		EnvFilePath: "/home/dev/.config/dbgorilla/collector/collector.env",
	}
}

func TestRunArgs_darwinNoAddHost(t *testing.T) {
	setGOOS(t, "darwin")
	args := newRunner().runArgs()
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--add-host") {
		t.Errorf("non-linux must not add host-gateway mapping: %s", joined)
	}
	for _, want := range []string{"run", "-d", "--name", DefaultContainerName, "--restart", "unless-stopped", "--env-file", "--config-file"} {
		if !strings.Contains(joined, want) {
			t.Errorf("runArgs missing %q: %s", want, joined)
		}
	}
	// Config is bind-mounted read-only.
	if !strings.Contains(joined, ":/etc/dbg-collector/collector.toml:ro") {
		t.Errorf("config mount missing: %s", joined)
	}
	// No CA cert -> no SSL_CERT_FILE.
	if strings.Contains(joined, "SSL_CERT_FILE") {
		t.Errorf("unexpected CA wiring without CACertPath: %s", joined)
	}
}

func TestRunArgs_linuxAddsHostGateway(t *testing.T) {
	setGOOS(t, "linux")
	args := newRunner().runArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--add-host="+DockerHostInternal+":host-gateway") {
		t.Errorf("linux must add host-gateway mapping: %s", joined)
	}
}

func TestRunArgs_withCACert(t *testing.T) {
	setGOOS(t, "darwin")
	r := newRunner()
	r.CACertPath = "/home/dev/ca.pem"
	args := r.runArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/home/dev/ca.pem:"+containerCAPath+":ro") {
		t.Errorf("CA cert mount missing: %s", joined)
	}
	if !strings.Contains(joined, "SSL_CERT_FILE="+containerCAPath) {
		t.Errorf("SSL_CERT_FILE env missing: %s", joined)
	}
	// Image, then the --config-file flag and its path, trail the argument list.
	n := len(args)
	if args[n-3] != r.Image || args[n-2] != "--config-file" || args[n-1] != "/etc/dbg-collector/collector.toml" {
		t.Errorf("image/config-file not at end of argv: %v", args)
	}
}

func TestRunCommandString(t *testing.T) {
	setGOOS(t, "darwin")
	s := newRunner().RunCommandString()
	if !strings.HasPrefix(s, "docker run ") {
		t.Errorf("RunCommandString should start with 'docker run ', got %q", s)
	}
	// Secrets ride --env-file, so the printable form must not leak them.
	if strings.Contains(s, "DBG_SERVER_SECRET=") || strings.Contains(s, "COLLECTOR_DB_PASSWORD=") {
		t.Errorf("RunCommandString leaked a secret: %s", s)
	}
}

func TestRunnerLifecycle_success(t *testing.T) {
	fakeDocker(t)
	setGOOS(t, "darwin")
	r := newRunner()
	for name, fn := range map[string]func() error{
		"Run":     r.Run,
		"Start":   r.Start,
		"Stop":    r.Stop,
		"Restart": r.Restart,
		"Remove":  r.Remove,
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

func TestRunner_dockerErrorIsWrapped(t *testing.T) {
	fakeDocker(t)
	setGOOS(t, "darwin")
	t.Setenv("HP_GENERIC_EXIT", "1")
	t.Setenv("HP_GENERIC_STDERR", "Error: No such container: dbg-collector")

	err := newRunner().Stop()
	if err == nil {
		t.Fatal("expected error when docker exits non-zero")
	}
	if !strings.Contains(err.Error(), "docker stop failed") {
		t.Errorf("error should name the failed subcommand, got %v", err)
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("error should include docker output, got %v", err)
	}
}

func TestLogs_success(t *testing.T) {
	fakeDocker(t)
	if err := newRunner().Logs(true, "100"); err != nil {
		t.Errorf("Logs(follow,tail): %v", err)
	}
	if err := newRunner().Logs(false, ""); err != nil {
		t.Errorf("Logs(no-follow,no-tail): %v", err)
	}
}

func TestLogs_failure(t *testing.T) {
	fakeDocker(t)
	t.Setenv("HP_GENERIC_EXIT", "1")
	if err := newRunner().Logs(false, ""); err == nil {
		t.Error("expected error when docker logs exits non-zero")
	}
}

func TestRunning(t *testing.T) {
	tests := []struct {
		name        string
		out         string
		stderr      string
		exit        string
		wantExists  bool
		wantRunning bool
	}{
		{name: "running", out: "true", wantExists: true, wantRunning: true},
		{name: "stopped", out: "false", wantExists: true, wantRunning: false},
		{name: "absent", stderr: "Error: No such object: dbg-collector", exit: "1", wantExists: false, wantRunning: false},
		{name: "other-error", stderr: "some transient docker error", exit: "1", wantExists: false, wantRunning: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDocker(t)
			t.Setenv("HP_INSPECT_RUNNING_OUT", tc.out)
			t.Setenv("HP_INSPECT_RUNNING_STDERR", tc.stderr)
			t.Setenv("HP_INSPECT_RUNNING_EXIT", tc.exit)

			exists, running, err := newRunner().Running()
			if err != nil {
				t.Fatalf("Running returned err: %v", err)
			}
			if exists != tc.wantExists || running != tc.wantRunning {
				t.Errorf("Running = (exists=%v, running=%v), want (%v, %v)",
					exists, running, tc.wantExists, tc.wantRunning)
			}
		})
	}
}

func TestImageRef(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_IMAGE_OUT", DefaultImage)
		if got := newRunner().ImageRef(); got != DefaultImage {
			t.Errorf("ImageRef = %q, want %q", got, DefaultImage)
		}
	})
	t.Run("failure-returns-empty", func(t *testing.T) {
		fakeDocker(t)
		t.Setenv("HP_INSPECT_IMAGE_EXIT", "1")
		if got := newRunner().ImageRef(); got != "" {
			t.Errorf("ImageRef on failure = %q, want empty", got)
		}
	})
}
