package cmd

import (
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// `collector upgrade` installs the version compiled into whichever dbg runs
// it. An out-of-date dbg therefore rolls a newer collector backwards — and
// used to print "✓ Upgraded" while doing it. These pin the guard.

func upgradeTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := baseCmd()
	c.Flags().String("image", collector.DefaultImage, "")
	c.Flags().Bool("allow-downgrade", false, "")
	return c
}

const (
	repoRef = "dbgorillapublic.azurecr.io/dbg-collector"
	newer   = repoRef + ":0.4.0"
	older   = repoRef + ":0.3.3"
)

func TestCheckUpgradeDirection_RefusesADowngrade(t *testing.T) {
	var err error
	var done bool
	out := capture(t, func() { done, err = checkUpgradeDirection(upgradeTestCmd(t), newer, older) })

	if err == nil {
		t.Fatal("installing an older version than the one running must be refused")
	}
	if done {
		t.Error("a refusal is not a clean exit")
	}
	// The operator has to be able to see what happened and what to do.
	for _, want := range []string{"0.4.0", "0.3.3", "dbg upgrade", "--allow-downgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got:\n%v", want, err)
		}
	}
	if out != "" {
		t.Errorf("nothing should be printed before the refusal, got %q", out)
	}
}

func TestCheckUpgradeDirection_AllowsAnExplicitDowngrade(t *testing.T) {
	c := upgradeTestCmd(t)
	mustSet(t, c, "allow-downgrade", "true")

	var err error
	var done bool
	out := capture(t, func() { done, err = checkUpgradeDirection(c, newer, older) })
	if err != nil || done {
		t.Fatalf("an explicit --allow-downgrade must proceed, got (done=%v, err=%v)", done, err)
	}
	// It still says so out loud — a deliberate downgrade is worth a line.
	if !strings.Contains(out, "Downgrading") {
		t.Errorf("out = %q, want the downgrade stated", out)
	}
}

// Rebuilding the container to install what is already running is downtime for
// no change.
func TestCheckUpgradeDirection_AlreadyCurrentDoesNothing(t *testing.T) {
	var err error
	var done bool
	out := capture(t, func() { done, err = checkUpgradeDirection(upgradeTestCmd(t), newer, newer) })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !done {
		t.Fatal("an already-current collector must not be rebuilt")
	}
	if !strings.Contains(out, "nothing to upgrade") {
		t.Errorf("out = %q, want it to say so plainly", out)
	}
}

func TestCheckUpgradeDirection_RealUpgradeProceeds(t *testing.T) {
	done, err := checkUpgradeDirection(upgradeTestCmd(t), older, newer)
	if err != nil || done {
		t.Fatalf("a genuine upgrade must proceed, got (done=%v, err=%v)", done, err)
	}
}

// An unrecognisable pair must not block: refusing here would break every
// upgrade to a locally-built or custom image, and to a collector installed
// before the state file recorded an image at all.
func TestCheckUpgradeDirection_UnknownComparisonProceeds(t *testing.T) {
	cases := []struct{ name, current, target string }{
		{"a custom image", "ghcr.io/acme/collector:1.0", newer},
		{"a floating tag", repoRef + ":latest", newer},
		{"no recorded image", "", newer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done, err := checkUpgradeDirection(upgradeTestCmd(t), c.current, c.target)
			if err != nil || done {
				t.Errorf("must not block an uncomparable pair, got (done=%v, err=%v)", done, err)
			}
		})
	}
}

// The end-to-end guarantee: a stale CLI does not touch the container.
func TestRunCollectorUpgrade_StaleCLIDoesNotReplaceANewerCollector(t *testing.T) {
	isolate(t)
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-1", Target: "docker", ContainerName: "dbg-collector", Image: newer,
	}); err != nil {
		t.Fatal(err)
	}

	c := upgradeTestCmd(t)
	mustSet(t, c, "image", older)

	var err error
	capture(t, func() { err = runCollectorUpgrade(c, nil) })
	if err == nil {
		t.Fatal("expected the downgrade refusal")
	}
	// The recorded image must be untouched — the collector was never replaced.
	st, _ := collector.LoadState()
	if st == nil || st.Image != newer {
		t.Errorf("state image = %+v, want %q unchanged", st, newer)
	}
}

// The default image is a moving tag now, so the image the CLI resolves is not
// something that can be compared against what is running. Pinning it to a
// digest first is what makes "already on this" detectable -- otherwise every
// upgrade run removes and recreates a healthy container to install the image
// it is already running.
func TestRunCollectorUpgrade_LatestAlreadyRunningIsANoOp(t *testing.T) {
	isolate(t)
	const digest = "sha256:783f3f13899f1bb87952bffa07b8bea78c249b6365d72f639a28446f5817c261"
	stubPinnedRef(t, repoRef+":latest@"+digest, nil)

	replaced := false
	orig := runContainer
	runContainer = func(collector.Runner) error { replaced = true; return nil }
	t.Cleanup(func() { runContainer = orig })

	// Running the same digest under its version tag.
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-1", Target: "docker", ContainerName: "dbg-collector",
		Image: repoRef + ":0.5.0@" + digest,
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	out := capture(t, func() { err = runCollectorUpgrade(upgradeTestCmd(t), nil) })
	if err != nil {
		t.Fatalf("runCollectorUpgrade: %v", err)
	}
	if replaced {
		t.Error("the container must not be rebuilt to install what it is already running")
	}
	if !strings.Contains(out, "nothing to upgrade") {
		t.Errorf("out = %q, want it to say so plainly", out)
	}
}

// A genuinely newer digest under the moving tag still upgrades.
func TestRunCollectorUpgrade_LatestWithANewDigestProceeds(t *testing.T) {
	isolate(t)
	stubPinnedRef(t, repoRef+":latest@sha256:newdigest", nil)

	replaced := false
	orig := runContainer
	runContainer = func(collector.Runner) error { replaced = true; return nil }
	t.Cleanup(func() { runContainer = orig })

	if err := collector.SaveState(&collector.State{
		AgentID: "agent-1", Target: "docker", ContainerName: "dbg-collector",
		Image: repoRef + ":0.5.0@sha256:olddigest",
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	capture(t, func() { err = runCollectorUpgrade(upgradeTestCmd(t), nil) })
	if err != nil {
		t.Fatalf("runCollectorUpgrade: %v", err)
	}
	if !replaced {
		t.Error("a different digest is a real upgrade and must be installed")
	}
	st, _ := collector.LoadState()
	if st == nil || st.Image != repoRef+":latest@sha256:newdigest" {
		t.Errorf("state should record the pinned digest, got %+v", st)
	}
}
