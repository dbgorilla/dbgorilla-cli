package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// --- target dispatch ------------------------------------------------------

func TestRunInstall_TargetDispatch(t *testing.T) {
	isolate(t)
	writeTokens(t)

	t.Run("an unknown target names the valid ones", func(t *testing.T) {
		c := awsCmd(t)
		mustSet(t, c, "target", "gcp")
		err := runInstall(c, nil)
		if err == nil {
			t.Fatal("an unknown target must be rejected")
		}
		for _, want := range []string{"gcp", "docker", "aws"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	})

	// Both spellings of each target must route the same way, or a documented
	// flag value silently installs the wrong thing.
	t.Run("aws and fargate both take the AWS path", func(t *testing.T) {
		for _, target := range []string{"aws", "fargate"} {
			stubAwsAvailable(t, errors.New("sentinel-aws-path"))
			c := awsCmd(t)
			mustSet(t, c, "target", target)
			mustSet(t, c, "yes", "true")
			var err error
			capture(t, func() { err = runInstall(c, nil) })
			if err == nil || !strings.Contains(err.Error(), "sentinel-aws-path") {
				t.Errorf("--target %s should route to the AWS installer, got %v", target, err)
			}
		}
	})

	t.Run("docker, local and empty all take the local path", func(t *testing.T) {
		for _, target := range []string{"docker", "local", ""} {
			origD := dockerAvailable
			dockerAvailable = func() error { return errors.New("sentinel-local-path") }
			c := awsCmd(t)
			c.Flags().String("ca-cert", "", "")
			mustSet(t, c, "target", target)
			mustSet(t, c, "yes", "true")
			var err error
			capture(t, func() { err = runInstall(c, nil) })
			dockerAvailable = origD
			if err == nil || !strings.Contains(err.Error(), "sentinel-local-path") {
				t.Errorf("--target %q should route to the local installer, got %v", target, err)
			}
		}
	})
}

// --- status ---------------------------------------------------------------

func TestRunStatus_NothingInstalled(t *testing.T) {
	isolate(t)
	out := capture(t, func() {
		if err := runStatus(awsCmd(t), nil); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, "No collector installed") {
		t.Errorf("out = %q", out)
	}
	// It has to say how to fix it, not just that nothing is there.
	if !strings.Contains(out, "collector install") {
		t.Errorf("out = %q, want the next command", out)
	}
}

// An AWS collector reports its stack, not a container.
func TestRunStatus_AWSStateUsesTheStackReport(t *testing.T) {
	isolate(t)
	stubStackStatus(t, "UPDATE_COMPLETE", nil)
	if err := collector.SaveState(&collector.State{
		AgentID: "agent-aws", Target: "aws", StackName: "dbg-collector", Region: "us-east-1",
	}); err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() {
		if err := runStatus(awsCmd(t), nil); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, "UPDATE_COMPLETE") {
		t.Errorf("out = %q, want the stack status", out)
	}
	if strings.Contains(out, "Container:") {
		t.Errorf("an AWS collector has no container line, got: %s", out)
	}
}

// --- connection verification ---------------------------------------------

func TestVerifyConnection_ReportsWhenConnected(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"connected"}`))
	}))
	defer srv.Close()
	writeTokens(t)

	client := api.NewClient(srv.URL)
	out := capture(t, func() { verifyConnection(client, "agent-x", "") })
	if !strings.Contains(out, "Collector connected") {
		t.Errorf("out = %q", out)
	}
}

// --- prompt ---------------------------------------------------------------

func TestPrompt_DefaultsAndOverrides(t *testing.T) {
	t.Run("empty input takes the default", func(t *testing.T) {
		setStdin(t, "\n")
		var got string
		out := capture(t, func() { got = prompt("Name", "fallback") })
		if got != "fallback" {
			t.Errorf("got %q, want the default", got)
		}
		if !strings.Contains(out, "fallback") {
			t.Errorf("the default should be shown, got %q", out)
		}
	})

	t.Run("typed input wins and is trimmed", func(t *testing.T) {
		setStdin(t, "  typed  \n")
		var got string
		capture(t, func() { got = prompt("Name", "fallback") })
		if got != "typed" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("no default renders a plain label", func(t *testing.T) {
		setStdin(t, "value\n")
		out := capture(t, func() { _ = prompt("Name", "") })
		if strings.Contains(out, "[]") {
			t.Errorf("an empty default should not render brackets, got %q", out)
		}
	})
}

func TestConfirm(t *testing.T) {
	t.Run("--yes answers yes without asking", func(t *testing.T) {
		c := awsCmd(t)
		mustSet(t, c, "yes", "true")
		setStdin(t, "") // nothing to read; it must not block
		if !confirm(c, "Continue?") {
			t.Error("--yes must confirm")
		}
	})

	t.Run("y confirms", func(t *testing.T) {
		setStdin(t, "y\n")
		var got bool
		capture(t, func() { got = confirm(awsCmd(t), "Continue?") })
		if !got {
			t.Error("y should confirm")
		}
	})

	// The default is no: an unanswered prompt must not proceed.
	t.Run("anything else declines", func(t *testing.T) {
		for _, in := range []string{"\n", "n\n", "maybe\n"} {
			setStdin(t, in)
			var got bool
			capture(t, func() { got = confirm(awsCmd(t), "Continue?") })
			if got {
				t.Errorf("%q must not confirm", in)
			}
		}
	})
}
