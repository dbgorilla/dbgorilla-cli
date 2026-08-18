package collector

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Seams for tests. These default to the stdlib implementations so production
// behavior is unchanged; tests substitute fakes to avoid invoking a real
// docker daemon or depending on the host OS.
var (
	execCommand  = exec.Command
	execLookPath = exec.LookPath
	goos         = runtime.GOOS
)

// ImageRepo is the published collector repository, used to build an image ref
// from a deployment-advertised preferred version (`<ImageRepo>:<version>`).
const ImageRepo = "dbgorillapublic.azurecr.io/dbg-collector"

// DefaultImage is the newest published collector.
//
// A moving tag rather than a fixed version: a version baked in here is only as
// current as the binary holding it, so `collector upgrade` could not move a
// collector forward once this CLI fell behind.
//
// Reproducibility is kept where it matters. PinnedRef resolves this to an
// immutable digest before anything runs locally, so an install records exactly
// what it started. Pass `--image <repo>:<version>` for a specific version.
const DefaultImage = ImageRepo + ":latest"

// ImageForVersion returns the image ref for a deployment-blessed version string.
func ImageForVersion(version string) string {
	return ImageRepo + ":" + version
}

// PinnedRef ensures ref is pinned to an immutable digest before we run it.
// If ref already carries an @sha256 digest (e.g. the built-in DefaultImage) it
// is returned unchanged. Otherwise — a deployment-blessed version like
// "<repo>:0.2.0" or a bare --image tag — the image is pulled and its repo
// digest resolved, yielding "<ref>@sha256:...". This keeps a centrally
// rolled-out version as reproducible and tamper-evident as a hard-pinned
// default (a tag is mutable; a digest is not).
func PinnedRef(ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	if err := docker("pull", ref); err != nil {
		return "", err
	}
	out, err := execCommand("docker", "inspect", "-f", "{{index .RepoDigests 0}}", ref).Output()
	if err != nil {
		return "", fmt.Errorf("cannot resolve image digest for %s (is it pushed to a registry?): %w", ref, err)
	}
	return pinnedRefFrom(ref, strings.TrimSpace(string(out)))
}

// pinnedRefFrom combines a human-readable ref (repo:tag) with the digest from a
// "repo@sha256:..." RepoDigests entry, yielding "repo:tag@sha256:...". Split
// out from PinnedRef so the string logic is unit-testable without a docker
// daemon.
func pinnedRefFrom(ref, repoDigest string) (string, error) {
	_, digest, found := strings.Cut(repoDigest, "@")
	if !found || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("unexpected repo digest %q for %s", repoDigest, ref)
	}
	return ref + "@" + digest, nil
}

// DefaultContainerName is the stable name for the local collector container.
const DefaultContainerName = "dbg-collector"

// DockerAvailable returns nil when a usable Docker engine is reachable.
func DockerAvailable() error {
	if _, err := execLookPath("docker"); err != nil {
		return fmt.Errorf("docker not found on PATH. Install Docker Desktop or Colima, then retry")
	}
	if out, err := execCommand("docker", "info").CombinedOutput(); err != nil {
		return fmt.Errorf("docker is installed but the engine is not responding (is it running?):\n%s",
			strings.TrimSpace(string(out)))
	}
	return nil
}

// Runner drives the collector container's lifecycle via the docker CLI.
type Runner struct {
	Name        string
	Image       string
	ConfigPath  string
	EnvFilePath string
	// CACertPath, when set, is a PEM CA bundle mounted into the container and
	// pointed at via SSL_CERT_FILE so the collector trusts a private/internal
	// CA (e.g. on-prem or internal deployments). Note: this replaces the
	// system trust bundle inside the container, so it is intended for
	// deployments whose endpoints all chain to this CA.
	CACertPath string
}

// containerCAPath is where the user's CA bundle is mounted inside the container.
const containerCAPath = "/etc/ssl/certs/dbg-collector-ca.pem"

// runArgs builds the `docker run` argument list. On Linux we add the
// host-gateway mapping so host.docker.internal resolves (it is native on
// Docker Desktop). Secrets arrive via --env-file, never on argv.
func (r Runner) runArgs() []string {
	args := []string{"run", "-d", "--name", r.Name, "--restart", "unless-stopped"}
	if goos == "linux" {
		args = append(args, "--add-host="+DockerHostInternal+":host-gateway")
	}
	args = append(args,
		"--env-file", r.EnvFilePath,
		"-v", r.ConfigPath+":/etc/dbg-collector/collector.toml:ro",
	)
	if r.CACertPath != "" {
		args = append(args,
			"-v", r.CACertPath+":"+containerCAPath+":ro",
			"-e", "SSL_CERT_FILE="+containerCAPath,
		)
	}
	args = append(args,
		r.Image,
		"--config-file", "/etc/dbg-collector/collector.toml",
	)
	return args
}

// Run starts the collector container detached.
func (r Runner) Run() error {
	return docker(r.runArgs()...)
}

// RunCommandString returns the printable `docker run ...` invocation, for
// dry-run output. Secrets are not on argv (they ride --env-file), so this is
// safe to display.
func (r Runner) RunCommandString() string {
	return "docker " + strings.Join(r.runArgs(), " ")
}

// Start (re)starts an existing stopped container.
func (r Runner) Start() error { return docker("start", r.Name) }

// Stop stops the running container.
func (r Runner) Stop() error { return docker("stop", r.Name) }

// Restart restarts the container.
func (r Runner) Restart() error { return docker("restart", r.Name) }

// Remove force-removes the container (stopping it first if needed).
func (r Runner) Remove() error { return docker("rm", "-f", r.Name) }

// Logs streams container logs to stdout/stderr. When follow is true it blocks
// until interrupted.
func (r Runner) Logs(follow bool, tail string) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	if tail != "" {
		args = append(args, "--tail", tail)
	}
	args = append(args, r.Name)
	cmd := execCommand("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Running reports whether the container exists and is currently running.
func (r Runner) Running() (exists bool, running bool, err error) {
	out, err := execCommand("docker", "inspect", "-f", "{{.State.Running}}", r.Name).CombinedOutput()
	if err != nil {
		// inspect exits non-zero when the container does not exist.
		if strings.Contains(string(out), "No such object") {
			return false, false, nil
		}
		return false, false, nil
	}
	return true, strings.TrimSpace(string(out)) == "true", nil
}

// Health reports the container's docker state and how many times it has
// restarted. A container that is restart-looping (crash on boot, e.g. an
// unreadable config mount) reports State "restarting" with a climbing
// RestartCount, which from the outside looks nothing like a healthy start.
func (r Runner) Health() (state string, restarts int, err error) {
	out, err := execCommand("docker", "inspect", "-f", "{{.State.Status}} {{.RestartCount}}", r.Name).CombinedOutput()
	if err != nil {
		return "", 0, fmt.Errorf("cannot inspect container %s: %w", r.Name, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("unexpected docker inspect output for %s: %q", r.Name, strings.TrimSpace(string(out)))
	}
	n, convErr := strconv.Atoi(fields[1])
	if convErr != nil {
		return fields[0], 0, nil
	}
	return fields[0], n, nil
}

// RecentLogs returns the last n lines of the container's logs as a string, for
// embedding in a diagnostic message. Runner.Logs streams to stdout instead.
func (r Runner) RecentLogs(n int) string {
	out, err := execCommand("docker", "logs", "--tail", strconv.Itoa(n), r.Name).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ImageRef returns the image the container was created from (for status).
func (r Runner) ImageRef() string {
	out, err := execCommand("docker", "inspect", "-f", "{{.Config.Image}}", r.Name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// docker runs a docker subcommand, returning a wrapped error with output on
// failure.
func docker(args ...string) error {
	out, err := execCommand("docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s failed: %w\n%s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
