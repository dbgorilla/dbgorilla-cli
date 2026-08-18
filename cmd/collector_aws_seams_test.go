package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// Fakes for the AWS seams. Each swaps one operation that would otherwise reach
// a live AWS account and restores it on cleanup.

// stubAWSOK makes every AWS precondition succeed with plausible values, so a
// test only has to override the one seam it is actually about.
func stubAWSOK(t *testing.T) {
	t.Helper()
	stubAwsAvailable(t, nil)
	stubAwsIdentity(t, "arn:aws:iam::111122223333:user/dev", nil)
	stubAwsAccount(t, "111122223333", nil)
	stubAwsRegion(t, "us-east-1")
	stubNetworkPath(t, nil, nil)
	stubRemoteDigest(t, nil)
}

func stubAwsAvailable(t *testing.T, err error) {
	t.Helper()
	orig := awsAvailable
	awsAvailable = func() error { return err }
	t.Cleanup(func() { awsAvailable = orig })
}

func stubAwsIdentity(t *testing.T, arn string, err error) {
	t.Helper()
	orig := awsIdentity
	awsIdentity = func() (string, error) { return arn, err }
	t.Cleanup(func() { awsIdentity = orig })
}

func stubAwsAccount(t *testing.T, acct string, err error) {
	t.Helper()
	orig := awsAccountID
	awsAccountID = func() (string, error) { return acct, err }
	t.Cleanup(func() { awsAccountID = orig })
}

func stubAwsRegion(t *testing.T, region string) {
	t.Helper()
	orig := awsRegion
	awsRegion = func() string { return region }
	t.Cleanup(func() { awsRegion = orig })
}

// stubDiscover answers every discovery call with target (or err).
func stubDiscover(t *testing.T, target collector.AwsTarget, err error) {
	t.Helper()
	orig := discoverAwsTarget
	discoverAwsTarget = func(id, provider string, into collector.AwsTarget) (collector.AwsTarget, error) {
		if err != nil {
			return into, err
		}
		out := target
		if id != "" {
			out.InstanceID = id
		}
		if provider != "" {
			out.ProviderType = provider
		}
		// Explicit fields on the seed still win, mirroring the real merge.
		if len(into.Databases) > 0 {
			out.Databases = into.Databases
		}
		if into.User != "" {
			out.User = into.User
		}
		if into.AuthMethod != "" {
			out.AuthMethod = into.AuthMethod
		}
		return out, nil
	}
	t.Cleanup(func() { discoverAwsTarget = orig })
}

func stubStackStatus(t *testing.T, status string, err error) {
	t.Helper()
	orig := stackStatus
	stackStatus = func(string, string) (string, error) { return status, err }
	t.Cleanup(func() { stackStatus = orig })
}

// stubUpdateComponents records the arguments an in-place update was called with.
type updateCall struct {
	called        bool
	stack, region string
	targets       []collector.AwsTarget
	password      string
}

func stubUpdateComponents(t *testing.T, err error) *updateCall {
	t.Helper()
	rec := &updateCall{}
	orig := updateComponents
	updateComponents = func(stack, region string, targets []collector.AwsTarget, password string) error {
		rec.called, rec.stack, rec.region, rec.targets, rec.password = true, stack, region, targets, password
		return err
	}
	t.Cleanup(func() { updateComponents = orig })
	return rec
}

// stubDeploy records deploys and controls their outcome.
type deployCall struct {
	count  int
	params map[string]string
	stack  string
	dryRun bool
}

func stubDeploy(t *testing.T, err error) *deployCall {
	t.Helper()
	rec := &deployCall{}
	origRun, origQuiet := runFargateDeploy, runFargateDeployQuiet
	runFargateDeploy = func(d collector.FargateDeploy) error {
		rec.count++
		rec.params, rec.stack, rec.dryRun = d.Params, d.StackName, d.DryRun
		return err
	}
	runFargateDeployQuiet = func(d collector.FargateDeploy) (string, error) {
		rec.count++
		rec.params, rec.stack, rec.dryRun = d.Params, d.StackName, d.DryRun
		return "", err
	}
	t.Cleanup(func() {
		runFargateDeploy, runFargateDeployQuiet = origRun, origQuiet
	})
	return rec
}

func stubDeleteStack(t *testing.T, err error) *bool {
	t.Helper()
	called := new(bool)
	orig := deleteStack
	deleteStack = func(string, string) error { *called = true; return err }
	t.Cleanup(func() { deleteStack = orig })
	return called
}

func stubUpgradeImage(t *testing.T, err error) *string {
	t.Helper()
	got := new(string)
	orig := upgradeImage
	upgradeImage = func(_, _, image string) error { *got = image; return err }
	t.Cleanup(func() { upgradeImage = orig })
	return got
}

func stubNetworkPath(t *testing.T, findings []collector.NetworkFinding, err error) {
	t.Helper()
	orig := checkNetworkPath
	checkNetworkPath = func(context.Context, string, []string, []collector.AwsTarget) ([]collector.NetworkFinding, error) {
		return findings, err
	}
	t.Cleanup(func() { checkNetworkPath = orig })
}

func stubRunGrant(t *testing.T, err error) *int {
	t.Helper()
	n := new(int)
	orig := runGrant
	runGrant = func(context.Context, string, []string) error { *n++; return err }
	t.Cleanup(func() { runGrant = orig })
	return n
}

func stubReachable(t *testing.T, err error) {
	t.Helper()
	orig := reachable
	reachable = func(string) error { return err }
	t.Cleanup(func() { reachable = orig })
}

// stubRemoteDigest replaces the registry digest lookup. The AWS path resolves
// a tag over the registry's HTTP API, which a test must never actually reach.
func stubRemoteDigest(t *testing.T, err error) {
	t.Helper()
	orig := pinImageRemote
	pinImageRemote = func(ref string) (string, error) {
		if err != nil {
			return "", err
		}
		if strings.Contains(ref, "@sha256:") {
			return ref, nil
		}
		return ref + "@sha256:testdigest", nil
	}
	t.Cleanup(func() { pinImageRemote = orig })
}

// awsCmd builds a command carrying the flags the AWS install path reads.
func awsCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := baseCmd()
	c.Flags().String("target", "aws", "")
	c.Flags().String("name", "", "")
	c.Flags().String("db-instance-id", "", "")
	c.Flags().String("dbi-resource-id", "", "")
	c.Flags().String("db-name", "", "")
	c.Flags().String("db-user", "", "")
	c.Flags().String("db-password", "", "")
	c.Flags().String("ssl-mode", "verify-full", "")
	c.Flags().String("provider-type", "", "")
	c.Flags().String("subnets", "", "")
	c.Flags().String("security-group-id", "", "")
	c.Flags().String("stack-name", "dbg-collector", "")
	c.Flags().String("assign-public-ip", "DISABLED", "")
	c.Flags().String("template-url", "", "")
	c.Flags().String("config", "", "")
	c.Flags().String("image", collector.DefaultImage, "")
	c.Flags().String("commands", "", "")
	c.Flags().Bool("enable-commands", true, "")
	c.Flags().Bool("run-grant", false, "")
	c.Flags().String("grant-user", "postgres", "")
	c.Flags().String("grant-password", "", "")
	c.Flags().Bool("force", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().String("auth-url", "", "")
	c.Flags().String("otlp-url", "", "")
	c.SetContext(context.Background())
	return c
}

// completeTarget is a fully-discovered database, so tests that are not about
// discovery can skip it.
func completeTarget() collector.AwsTarget {
	return collector.AwsTarget{
		Name:          "prod",
		InstanceID:    "prod-db",
		DbiResourceID: "db-RES",
		Host:          "prod-db.abc.us-east-1.rds.amazonaws.com",
		Port:          5432,
		User:          "dbgorilla_ro",
		Databases:     []string{"appdb"},
		Subnets:       []string{"subnet-a", "subnet-b"},
		SecurityGroup: "sg-db",
		ProviderType:  "aws_rds",
		IAMAuthOn:     true,
	}
}
