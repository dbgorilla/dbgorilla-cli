package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// The local upgrade path replaces a running container. Its failure branches
// decide whether a customer is left with no collector at all.

func stubPinnedRef(t *testing.T, ref string, err error) {
	t.Helper()
	orig := pinImage
	pinImage = func(string) (string, error) { return ref, err }
	t.Cleanup(func() { pinImage = orig })
}

func TestRunCollectorUpgrade_LocalReplacesTheContainer(t *testing.T) {
	isolate(t)
	stubPinnedRef(t, "img@sha256:abc", nil)
	ran := false
	orig := runContainer
	runContainer = func(collector.Runner) error { ran = true; return nil }
	t.Cleanup(func() { runContainer = orig })

	if err := collector.SaveState(&collector.State{
		AgentID: "agent-local", Target: "docker", ContainerName: "dbg-collector", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "image", "img:v2")
	var err error
	out := capture(t, func() { err = runCollectorUpgrade(c, nil) })
	if err != nil {
		t.Fatalf("runCollectorUpgrade: %v\n%s", err, out)
	}
	if !ran {
		t.Error("the replacement container must be started")
	}
	// The digest-pinned reference is what gets recorded, not the floating tag:
	// otherwise `status` claims a version the container may not be running.
	st, _ := collector.LoadState()
	if st == nil || st.Image != "img@sha256:abc" {
		t.Errorf("state image = %+v, want the pinned digest", st)
	}
}

func TestRunCollectorUpgrade_LocalPinFailureStops(t *testing.T) {
	isolate(t)
	stubPinnedRef(t, "", errors.New("manifest unknown"))
	ran := false
	orig := runContainer
	runContainer = func(collector.Runner) error { ran = true; return nil }
	t.Cleanup(func() { runContainer = orig })

	if err := collector.SaveState(&collector.State{
		AgentID: "agent-local", Target: "docker", ContainerName: "dbg-collector", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	capture(t, func() { err = runCollectorUpgrade(awsCmd(t), nil) })
	if err == nil {
		t.Fatal("an unresolvable image must stop the upgrade")
	}
	// Critically, the old container must not have been removed first.
	if ran {
		t.Error("nothing should be started when the image cannot be resolved")
	}
}

func TestRunCollectorUpgrade_LocalStartFailureSurfaces(t *testing.T) {
	isolate(t)
	stubPinnedRef(t, "img@sha256:abc", nil)
	orig := runContainer
	runContainer = func(collector.Runner) error { return errors.New("port already allocated") }
	t.Cleanup(func() { runContainer = orig })

	if err := collector.SaveState(&collector.State{
		AgentID: "agent-local", Target: "docker", ContainerName: "dbg-collector", Image: "img:v1",
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	capture(t, func() { err = runCollectorUpgrade(awsCmd(t), nil) })
	if err == nil || !strings.Contains(err.Error(), "port already allocated") {
		t.Fatalf("err = %v, want the docker error surfaced", err)
	}
	// The recorded image must not move when the new container never started.
	st, _ := collector.LoadState()
	if st.Image != "img:v1" {
		t.Errorf("state image = %q, want the old one", st.Image)
	}
}

func TestRequireState(t *testing.T) {
	t.Run("no collector installed is actionable", func(t *testing.T) {
		isolate(t)
		_, err := requireState()
		if err == nil {
			t.Fatal("expected an error with nothing installed")
		}
		if !strings.Contains(err.Error(), "install") {
			t.Errorf("error should name the next command, got: %v", err)
		}
	})

	t.Run("returns the saved state", func(t *testing.T) {
		isolate(t)
		if err := collector.SaveState(&collector.State{AgentID: "agent-1", Target: "docker"}); err != nil {
			t.Fatal(err)
		}
		st, err := requireState()
		if err != nil {
			t.Fatalf("requireState: %v", err)
		}
		if st.AgentID != "agent-1" {
			t.Errorf("state = %+v", st)
		}
	})
}

func TestResolveVersion_FallsBackToBuildInfo(t *testing.T) {
	// A released binary carries ldflag values; a `go install module@tag` build
	// does not, and used to report "dev" forever. The fallback reads the tag
	// the Go toolchain embeds.
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origV, origC, origD })

	Version, Commit, Date = "v1.2.3", "abc1234", "2026-01-01"
	v, c, d := resolveVersion()
	if v != "v1.2.3" || c != "abc1234" || d != "2026-01-01" {
		t.Errorf("released ldflags must be authoritative, got (%q,%q,%q)", v, c, d)
	}

	// Under `go test` the main module reports "(devel)", which stays "dev".
	Version = "dev"
	v, _, _ = resolveVersion()
	if v != "dev" && !strings.HasPrefix(v, "v") {
		t.Errorf("version = %q, want either dev or a real tag", v)
	}
}
