package collector

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// DefaultImage is the published GA collector image, pinned by digest for
// reproducibility. Bump this on each release (see the collector repo's
// "Releasing" docs). Override with `dbg collector install --image`. Used as the
// fallback when the deployment advertises no preferred version.
const DefaultImage = "dbgorillapublic.azurecr.io/dbg-collector:0.1.0@sha256:4874dfe63453d9335e17c37405e640b09d91090f8b78b46bbbb4adbe5337c77a"

// ImageRepo is the published collector repository, used to build an image ref
// from a deployment-advertised preferred version (`<ImageRepo>:<version>`).
const ImageRepo = "dbgorillapublic.azurecr.io/dbg-collector"

// ImageForVersion returns the image ref for a deployment-blessed version string.
func ImageForVersion(version string) string {
	return ImageRepo + ":" + version
}

// DefaultContainerName is the stable name for the local collector container.
const DefaultContainerName = "dbg-collector"

// DockerAvailable returns nil when a usable Docker engine is reachable.
func DockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found on PATH. Install Docker Desktop or Colima, then retry")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
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
	if runtime.GOOS == "linux" {
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
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Running reports whether the container exists and is currently running.
func (r Runner) Running() (exists bool, running bool, err error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", r.Name).CombinedOutput()
	if err != nil {
		// inspect exits non-zero when the container does not exist.
		if strings.Contains(string(out), "No such object") {
			return false, false, nil
		}
		return false, false, nil
	}
	return true, strings.TrimSpace(string(out)) == "true", nil
}

// ImageRef returns the image the container was created from (for status).
func (r Runner) ImageRef() string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.Config.Image}}", r.Name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// docker runs a docker subcommand, returning a wrapped error with output on
// failure.
func docker(args ...string) error {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s failed: %w\n%s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
