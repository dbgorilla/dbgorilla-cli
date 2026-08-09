package collector

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// This file provides a hermetic stand-in for the `docker` CLI. It uses the
// canonical os/exec testing pattern: execCommand is swapped to re-invoke the
// test binary itself (running TestHelperProcess), so no real docker daemon,
// network, or host state is ever touched. The child's behavior is steered by
// HP_* env vars set with t.Setenv, keyed by the docker subcommand.

// fakeDocker replaces execCommand with a func that spawns the test binary as a
// fake docker, and restores the original on cleanup.
func fakeDocker(t *testing.T) {
	t.Helper()
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
	t.Cleanup(func() { execCommand = orig })
}

// fakeLookPath replaces execLookPath and restores it on cleanup.
func fakeLookPath(t *testing.T, path string, err error) {
	t.Helper()
	orig := execLookPath
	execLookPath = func(string) (string, error) { return path, err }
	t.Cleanup(func() { execLookPath = orig })
}

// setGOOS overrides the goos seam and restores it on cleanup.
func setGOOS(t *testing.T, v string) {
	t.Helper()
	orig := goos
	goos = v
	t.Cleanup(func() { goos = orig })
}

// TestHelperProcess is not a real test; it is the subprocess entrypoint used by
// fakeDocker. It emulates `docker <subcommand>` using HP_* env vars and exits.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Recover the emulated argv (everything after the "--" separator).
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// args is now [docker, sub, rest...].
	if len(args) < 2 {
		os.Exit(2)
	}
	sub := args[1]

	// emit writes optional stdout/stderr for the given key and exits with the
	// configured code (default 0).
	emit := func(key, stdout string) {
		if v := os.Getenv(key + "_STDERR"); v != "" {
			os.Stderr.WriteString(v + "\n")
		}
		if stdout != "" {
			os.Stdout.WriteString(stdout + "\n")
		}
		code := 0
		if c := os.Getenv(key + "_EXIT"); c != "" {
			code, _ = strconv.Atoi(c)
		}
		os.Exit(code)
	}

	switch sub {
	case "info":
		emit("HP_INFO", os.Getenv("HP_INFO_OUT"))
	case "pull":
		emit("HP_PULL", "")
	case "inspect":
		var format string
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				format = args[i+1]
			}
		}
		switch {
		case strings.Contains(format, "RepoDigests"):
			emit("HP_INSPECT_DIGEST", os.Getenv("HP_INSPECT_DIGEST_OUT"))
		case strings.Contains(format, "State.Running"):
			emit("HP_INSPECT_RUNNING", os.Getenv("HP_INSPECT_RUNNING_OUT"))
		case strings.Contains(format, "Config.Image"):
			emit("HP_INSPECT_IMAGE", os.Getenv("HP_INSPECT_IMAGE_OUT"))
		default:
			os.Exit(0)
		}
	default:
		// run, start, stop, restart, rm, logs, ...
		emit("HP_GENERIC", os.Getenv("HP_GENERIC_OUT"))
	}
}
