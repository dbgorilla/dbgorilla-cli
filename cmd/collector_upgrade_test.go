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
