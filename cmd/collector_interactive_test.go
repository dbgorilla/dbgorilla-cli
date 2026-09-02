package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/preflight"
)

// Everything the CLI asks a human hangs off "is stdin a terminal". A test
// process never has one, so these branches — which decide whether TLS is
// dropped, which databases are monitored, and whether a grant is run — were
// only ever exercised by hand.

// asTerminal makes the interactive branches reachable and answers any form
// with answers.
func asTerminal(t *testing.T, answers string) {
	t.Helper()
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = orig })

	origIn, origOut, origAcc := formIO.in, formIO.out, formIO.accessible
	formIO.in, formIO.out, formIO.accessible = strings.NewReader(answers), &strings.Builder{}, true
	t.Cleanup(func() { formIO.in, formIO.out, formIO.accessible = origIn, origOut, origAcc })
}

// --- TLS: the branch that can put a password on the wire in clear text -----

func TestResolveTLSMode_RemoteWithoutTLSNeedsAnExplicitYes(t *testing.T) {
	isolate(t)
	stubProbe(t, preflight.TLSUnsupported)

	t.Run("declining aborts", func(t *testing.T) {
		asTerminal(t, "n\n")
		tgt := &collector.Target{Name: "t", Host: "db.example.com", Port: 5432, User: "ro"}
		var err error
		capture(t, func() { err = resolveTLSMode(sslCmd(t), tgt, "pw") })
		if err == nil {
			t.Fatal("declining must abort the install")
		}
		if tgt.SSLMode == "disable" {
			t.Error("a declined prompt must not drop TLS")
		}
	})

	t.Run("accepting drops TLS only after the consequence is stated", func(t *testing.T) {
		asTerminal(t, "y\n")
		tgt := &collector.Target{Name: "t", Host: "db.example.com", Port: 5432, User: "ro"}
		var err error
		out := capture(t, func() { err = resolveTLSMode(sslCmd(t), tgt, "pw") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if tgt.SSLMode != "disable" {
			t.Errorf("SSLMode = %q, want disable after an explicit yes", tgt.SSLMode)
		}
		// The user must have been told what they are agreeing to.
		if !strings.Contains(out, "clear text") {
			t.Errorf("the exposure must be stated in plain words, got %q", out)
		}
	})
}

// The dry run has to preview the same decision the real install would make, or
// it advertises a config that never gets deployed.
func TestPreviewTLSMode(t *testing.T) {
	isolate(t)

	t.Run("local database previews the automatic downgrade", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnsupported)
		tgt := &collector.Target{Name: "t", Host: "localhost", Port: 5432, User: "ro"}
		out := capture(t, func() { previewTLSMode(sslCmd(t), tgt) })
		if tgt.SSLMode != "disable" {
			t.Errorf("SSLMode = %q, want disable", tgt.SSLMode)
		}
		if !strings.Contains(out, "on this machine") {
			t.Errorf("out = %q, want the reason it is safe", out)
		}
	})

	// A remote database is never previewed as downgraded — the real install
	// would stop and ask.
	t.Run("remote database previews the question, not a downgrade", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnsupported)
		tgt := &collector.Target{Name: "t", Host: "db.example.com", Port: 5432, User: "ro"}
		out := capture(t, func() { previewTLSMode(sslCmd(t), tgt) })
		if tgt.SSLMode == "disable" {
			t.Error("a dry run must not advertise a silent remote downgrade")
		}
		if !strings.Contains(out, "confirm") {
			t.Errorf("out = %q, want a note that the real install asks", out)
		}
	})

	t.Run("a server that speaks TLS previews nothing", func(t *testing.T) {
		stubProbe(t, preflight.TLSSupported)
		tgt := &collector.Target{Name: "t", Host: "db.example.com", Port: 5432, User: "ro"}
		out := capture(t, func() { previewTLSMode(sslCmd(t), tgt) })
		if out != "" {
			t.Errorf("out = %q, want silence when TLS works", out)
		}
	})

	t.Run("an explicit --ssl-mode skips the probe", func(t *testing.T) {
		stubProbe(t, preflight.TLSUnsupported)
		c := sslCmd(t)
		mustSet(t, c, "ssl-mode", "require")
		tgt := &collector.Target{Name: "t", Host: "localhost", Port: 5432, User: "ro"}
		out := capture(t, func() { previewTLSMode(c, tgt) })
		if tgt.SSLMode == "disable" {
			t.Error("an explicit choice must be honored")
		}
		if out != "" {
			t.Errorf("out = %q, want silence", out)
		}
	})
}

// --- crash-loop detection -------------------------------------------------

func stubContainerHealth(t *testing.T, states []string, restarts []int, err error) {
	t.Helper()
	origH, origL := containerHealth, containerRecentLogs
	i := 0
	containerHealth = func(collector.Runner) (string, int, error) {
		if err != nil {
			return "", 0, err
		}
		j := i
		if j >= len(states) {
			j = len(states) - 1
		}
		i++
		return states[j], restarts[j], nil
	}
	containerRecentLogs = func(collector.Runner, int) string {
		return "config error: cannot read /etc/dbgorilla/collector.toml"
	}
	t.Cleanup(func() { containerHealth, containerRecentLogs = origH, origL })
}

func TestCrashLooping(t *testing.T) {
	runner := collector.Runner{Name: "dbg-collector"}

	// The whole point: a container dying every two seconds used to be reported
	// as a healthy start followed by a connection problem.
	t.Run("a restarting container is diagnosed, with its own output", func(t *testing.T) {
		stubContainerHealth(t, []string{"restarting"}, []int{3}, nil)
		var got bool
		out := capture(t, func() { got = crashLooping(runner) })
		if !got {
			t.Fatal("a restarting container must be reported as crash-looping")
		}
		if !strings.Contains(out, "cannot read") {
			t.Errorf("the container's own output should be shown, got: %s", out)
		}
		// The likely cause is a bind mount, not a private CA.
		if !strings.Contains(out, "bind mount") && !strings.Contains(out, "shared path") {
			t.Errorf("the likely cause should be named, got: %s", out)
		}
		if !strings.Contains(out, "startup failure") {
			t.Errorf("it must not read as a connection problem, got: %s", out)
		}
	})

	t.Run("a healthy container is not flagged", func(t *testing.T) {
		stubContainerHealth(t, []string{"running", "running"}, []int{0, 0}, nil)
		var got bool
		out := capture(t, func() { got = crashLooping(runner) })
		if got {
			t.Fatalf("a running container must not be flagged, out=%s", out)
		}
	})

	// A container that restarted once and settled is still a problem worth
	// naming, but only if it has actually moved.
	t.Run("a climbing restart count is flagged", func(t *testing.T) {
		stubContainerHealth(t, []string{"running"}, []int{5}, nil)
		var got bool
		capture(t, func() { got = crashLooping(runner) })
		if !got {
			t.Error("a non-zero restart count means it is not staying up")
		}
	})

	// If docker cannot be inspected we know nothing; crying wolf here would
	// fail installs that are fine.
	t.Run("an uninspectable container stays quiet", func(t *testing.T) {
		stubContainerHealth(t, nil, nil, errors.New("no such object"))
		var got bool
		out := capture(t, func() { got = crashLooping(runner) })
		if got {
			t.Error("an inspect failure must not be reported as a crash loop")
		}
		if out != "" {
			t.Errorf("out = %q, want silence", out)
		}
	})
}

// --- interactive install branches ----------------------------------------

// With no --db-instance-id and a real terminal, the ambiguous case becomes a
// picker rather than an error.
func TestResolveAwsTargets_AmbiguityBecomesAPicker(t *testing.T) {
	isolate(t)
	asTerminal(t, "1\n0\n") // pick the first, then confirm

	amb := &collector.AmbiguousTargetError{Choices: []collector.TargetChoice{{ID: "prod-db", ProviderType: "aws_rds"}, {ID: "staging-db", ProviderType: "aws_rds"}}}
	calls := 0
	orig := discoverAwsTarget
	discoverAwsTarget = func(id, provider string, into collector.AwsTarget) (collector.AwsTarget, error) {
		calls++
		if id == "" {
			return into, amb // auto-select is ambiguous
		}
		out := completeTarget()
		out.InstanceID = id
		return out, nil
	}
	t.Cleanup(func() { discoverAwsTarget = orig })

	got, err := resolveAwsTargets(awsCmd(t))
	if err != nil {
		t.Fatalf("resolveAwsTargets: %v", err)
	}
	if len(got) != 1 || got[0].InstanceID != "prod-db" {
		t.Fatalf("targets = %+v, want the picked database", got)
	}
	if calls < 2 {
		t.Error("the picked candidate should be discovery-completed")
	}
}

// Exactly one candidate needs no picker at all.
func TestResolveAwsTargets_SingleCandidateSkipsThePicker(t *testing.T) {
	isolate(t)
	asTerminal(t, "")
	stubDiscover(t, completeTarget(), nil)

	got, err := resolveAwsTargets(awsCmd(t))
	if err != nil {
		t.Fatalf("resolveAwsTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %+v", got)
	}
}

func TestDiscoverChoices_FailureNamesTheCandidate(t *testing.T) {
	isolate(t)
	stubDiscover(t, collector.AwsTarget{}, errors.New("no such instance"))

	_, err := discoverChoices([]collector.TargetChoice{{ID: "ghost", ProviderType: "aws_rds"}}, collector.AwsTarget{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want the candidate named", err)
	}
}

// pickTarget is the plain numbered list (not the huh picker) used by the
// single-database path.
func TestPickTarget(t *testing.T) {
	amb := &collector.AmbiguousTargetError{Choices: []collector.TargetChoice{
		{ID: "prod-db", ProviderType: "aws_rds"},
		{ID: "staging-db", ProviderType: "aws_rds"},
		{ID: "analytics", ProviderType: "aws_aurora"},
	}}

	t.Run("a valid number selects that candidate", func(t *testing.T) {
		setStdin(t, "3\n")
		var got collector.TargetChoice
		var err error
		out := capture(t, func() { got, err = pickTarget(amb) })
		if err != nil {
			t.Fatalf("pickTarget: %v", err)
		}
		if got.ID != "analytics" || got.ProviderType != "aws_aurora" {
			t.Errorf("choice = %+v, want the Aurora cluster", got)
		}
		if !strings.Contains(out, "--db-instance-id") {
			t.Errorf("the non-interactive escape hatch should be offered, got: %s", out)
		}
	})

	t.Run("an empty answer takes the default", func(t *testing.T) {
		setStdin(t, "\n")
		var got collector.TargetChoice
		var err error
		capture(t, func() { got, err = pickTarget(amb) })
		if err != nil {
			t.Fatalf("pickTarget: %v", err)
		}
		if got.ID != "prod-db" {
			t.Errorf("choice = %+v, want the first candidate", got)
		}
	})

	// Out of range or nonsense must not silently select something.
	for _, in := range []string{"9\n", "0\n", "banana\n"} {
		t.Run("rejects "+strings.TrimSpace(in), func(t *testing.T) {
			setStdin(t, in)
			var err error
			capture(t, func() { _, err = pickTarget(amb) })
			if err == nil {
				t.Fatalf("%q must not select a database", in)
			}
			if !strings.Contains(err.Error(), "--db-instance-id") {
				t.Errorf("error should name the way out, got: %v", err)
			}
		})
	}
}

// --- IAM fallback ---------------------------------------------------------

// When IAM auth is off, an interactive install offers password auth rather
// than only warning — otherwise the collector simply cannot connect.
func TestResolveAwsAuth_OffersPasswordWhenIAMIsOff(t *testing.T) {
	isolate(t)
	asTerminal(t, "")

	target := completeTarget()
	target.IAMAuthOn = false
	targets := []collector.AwsTarget{target}
	pw := ""

	c := awsCmd(t)
	var err error
	out := capture(t, func() { err = resolveAwsAuth(c, targets, &pw, false) })
	// The password prompt cannot be answered in line-based mode, so this ends
	// on the warn-and-confirm path; what matters is that the offer was made.
	if !strings.Contains(out, "IAM database authentication isn't enabled") {
		t.Errorf("the fallback should be offered, got: %s", out)
	}
	_ = err
}

func TestResolveAwsAuth_IAMEnabledIsSilent(t *testing.T) {
	isolate(t)
	targets := []collector.AwsTarget{completeTarget()} // IAMAuthOn = true
	pw := ""
	c := awsCmd(t)
	mustSet(t, c, "yes", "true")

	out := capture(t, func() {
		if err := resolveAwsAuth(c, targets, &pw, false); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	if strings.Contains(out, "not enabled") {
		t.Errorf("IAM is on; nothing should be warned, got: %s", out)
	}
}

// Non-interactive with IAM off: warn, then let --yes carry it through rather
// than hanging on a prompt nobody can answer.
func TestResolveAwsAuth_NonInteractiveWarnsAndProceeds(t *testing.T) {
	isolate(t)
	target := completeTarget()
	target.IAMAuthOn = false
	targets := []collector.AwsTarget{target}
	pw := ""

	c := awsCmd(t)
	mustSet(t, c, "yes", "true")
	var err error
	out := capture(t, func() { err = resolveAwsAuth(c, targets, &pw, false) })
	if err != nil {
		t.Fatalf("--yes should carry it through, got %v", err)
	}
	if !strings.Contains(out, "IAM DB authentication") {
		t.Errorf("the operator must be told how to fix it, got: %s", out)
	}
}

// --- grants: the interactive offer ----------------------------------------

// Offering to run a grant against a database that cannot be reached from here
// is worse than just printing the SQL.
func TestApplyGrants_UnreachableSkipsTheOffer(t *testing.T) {
	isolate(t)
	asTerminal(t, "")
	stubReachable(t, errors.New("i/o timeout"))
	grants := stubRunGrant(t, nil)

	out := capture(t, func() { applyGrants(awsCmd(t), []collector.AwsTarget{completeTarget()}) })
	if *grants != 0 {
		t.Error("must not attempt a grant against an unreachable database")
	}
	if !strings.Contains(out, "isn't reachable from here") {
		t.Errorf("the reason should be stated, got: %s", out)
	}
	if !strings.Contains(out, "GRANT") {
		t.Errorf("the SQL should be printed instead, got: %s", out)
	}
}

// An explicit --run-grant skips the reachability probe: the user may be on a
// bastion and know better.
func TestApplyGrants_ExplicitRunGrantSkipsTheProbe(t *testing.T) {
	isolate(t)
	asTerminal(t, "")
	probed := false
	orig := reachable
	reachable = func(string) error { probed = true; return nil }
	t.Cleanup(func() { reachable = orig })
	grants := stubRunGrant(t, nil)

	c := awsCmd(t)
	mustSet(t, c, "run-grant", "true")
	mustSet(t, c, "grant-password", "admin")
	capture(t, func() { applyGrants(c, []collector.AwsTarget{completeTarget()}) })

	if probed {
		t.Error("an explicit --run-grant should not gate on the probe")
	}
	if *grants != 1 {
		t.Errorf("grant ran %d times, want 1", *grants)
	}
}

// --- deploy under a spinner ----------------------------------------------

func TestDeployStack_InteractiveShowsCapturedOutputOnFailure(t *testing.T) {
	isolate(t)
	asTerminal(t, "")

	origQuiet := runFargateDeployQuiet
	runFargateDeployQuiet = func(collector.FargateDeploy) (string, error) {
		return "CREATE_FAILED: the image could not be pulled", errors.New("deploy failed")
	}
	t.Cleanup(func() { runFargateDeployQuiet = origQuiet })

	var err error
	out := capture(t, func() { err = deployStack(collector.FargateDeploy{StackName: "s"}, "Deploying…") })
	if err == nil {
		t.Fatal("expected the deploy error")
	}
	// The captured CloudFormation output is only worth hiding while it succeeds.
	if !strings.Contains(out, "could not be pulled") {
		t.Errorf("the captured output should be shown on failure, got: %s", out)
	}
}

func TestDeployStack_NonInteractiveStreams(t *testing.T) {
	isolate(t)
	quiet := false
	origRun, origQuiet := runFargateDeploy, runFargateDeployQuiet
	runFargateDeploy = func(collector.FargateDeploy) error { return nil }
	runFargateDeployQuiet = func(collector.FargateDeploy) (string, error) { quiet = true; return "", nil }
	t.Cleanup(func() { runFargateDeploy, runFargateDeployQuiet = origRun, origQuiet })

	if err := deployStack(collector.FargateDeploy{StackName: "s"}, "Deploying…"); err != nil {
		t.Fatalf("deployStack: %v", err)
	}
	// CI logs stay useful only if progress is streamed rather than swallowed.
	if quiet {
		t.Error("a non-interactive deploy must stream, not capture")
	}
}
