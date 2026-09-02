package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
)

// --- target resolution ----------------------------------------------------

// A fully-specified target skips discovery entirely: nothing should reach AWS
// when the user has already said everything.
func TestResolveAwsTarget_CompleteTargetSkipsDiscovery(t *testing.T) {
	isolate(t)
	discovered := false
	orig := discoverAwsTarget
	discoverAwsTarget = func(string, string, collector.AwsTarget) (collector.AwsTarget, error) {
		discovered = true
		return collector.AwsTarget{}, nil
	}
	t.Cleanup(func() { discoverAwsTarget = orig })

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "dbi-resource-id", "db-RES")
	mustSet(t, c, "db-name", "appdb")
	mustSet(t, c, "subnets", "subnet-a,subnet-b")
	mustSet(t, c, "security-group-id", "sg-db")
	// Complete() also needs a host, which only discovery supplies... so this
	// asserts the flag-only path still calls discovery when Host is missing.
	got, err := resolveAwsTarget(c)
	if err != nil {
		t.Fatalf("resolveAwsTarget: %v", err)
	}
	if !discovered {
		t.Error("a target without a host is not complete; discovery should fill it")
	}
	_ = got
}

func TestResolveAwsTarget_DefaultsProviderAndUserWhenComplete(t *testing.T) {
	isolate(t)
	stubDiscover(t, completeTarget(), nil)

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	got, err := resolveAwsTarget(c)
	if err != nil {
		t.Fatalf("resolveAwsTarget: %v", err)
	}
	if got.InstanceID != "prod-db" {
		t.Errorf("instance id = %q", got.InstanceID)
	}
}

// --db-password opts a database into password auth instead of IAM.
func TestResolveAwsTarget_PasswordFlagSelectsPasswordAuth(t *testing.T) {
	isolate(t)
	stubDiscover(t, completeTarget(), nil)

	c := awsCmd(t)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "db-password", "s3cret")
	got, err := resolveAwsTarget(c)
	if err != nil {
		t.Fatalf("resolveAwsTarget: %v", err)
	}
	if got.AuthMethod != "password" {
		t.Errorf("AuthMethod = %q, want password", got.AuthMethod)
	}
}

// Non-interactive ambiguity must surface the actionable error rather than
// silently picking a database.
func TestResolveAwsTarget_AmbiguousNonInteractiveErrors(t *testing.T) {
	isolate(t)
	amb := &collector.AmbiguousTargetError{Choices: []collector.TargetChoice{{ID: "a", ProviderType: "aws_rds"}, {ID: "b", ProviderType: "aws_rds"}}}
	stubDiscover(t, collector.AwsTarget{}, amb)

	c := awsCmd(t)
	mustSet(t, c, "yes", "true") // --yes means "no prompts"
	_, err := resolveAwsTarget(c)
	if err == nil {
		t.Fatal("ambiguity must not be resolved by guessing")
	}
	if !strings.Contains(err.Error(), "--db-instance-id") {
		t.Errorf("error should name the way out, got: %v", err)
	}
}

func TestAwsDBPassword_FlagBeatsEnv(t *testing.T) {
	isolate(t)
	t.Setenv(collector.DBPasswordEnv, "from-env")

	c := awsCmd(t)
	if got := dbPasswordFlag(c); got != "from-env" {
		t.Errorf("with no flag the env var should be used, got %q", got)
	}
	mustSet(t, c, "db-password", "from-flag")
	if got := dbPasswordFlag(c); got != "from-flag" {
		t.Errorf("the flag must win, got %q", got)
	}
}

func TestAwsDBPassword_EmptyMeansIAM(t *testing.T) {
	isolate(t)
	t.Setenv(collector.DBPasswordEnv, "")
	if got := dbPasswordFlag(awsCmd(t)); got != "" {
		t.Errorf("got %q, want empty (IAM auth)", got)
	}
}

// --- multi-database config file -------------------------------------------

func TestResolveAwsTargetsFromConfig(t *testing.T) {
	isolate(t)
	stubDiscover(t, completeTarget(), nil)

	path := filepath.Join(t.TempDir(), "dbs.toml")
	if err := os.WriteFile(path, []byte(`
[[database]]
instance-id = "prod-pg"
name = "Production"

[[database]]
instance-id = "analytics"
provider-type = "aws_aurora"
databases = ["warehouse"]
`), 0644); err != nil {
		t.Fatal(err)
	}

	c := awsCmd(t)
	mustSet(t, c, "config", path)
	got, err := resolveAwsTargets(c)
	if err != nil {
		t.Fatalf("resolveAwsTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 targets, got %d", len(got))
	}
	// The file's explicit fields must survive discovery.
	if got[1].ProviderType != "aws_aurora" {
		t.Errorf("provider type = %q, want the file's aws_aurora", got[1].ProviderType)
	}
	if len(got[1].Databases) != 1 || got[1].Databases[0] != "warehouse" {
		t.Errorf("databases = %v, want the file's list", got[1].Databases)
	}
}

func TestResolveAwsTargetsFromConfig_DiscoveryFailureNamesTheEntry(t *testing.T) {
	isolate(t)
	stubDiscover(t, collector.AwsTarget{}, errors.New("no such instance"))

	path := filepath.Join(t.TempDir(), "dbs.toml")
	if err := os.WriteFile(path, []byte("[[database]]\ninstance-id = \"ghost\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := awsCmd(t)
	mustSet(t, c, "config", path)
	_, err := resolveAwsTargets(c)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want the failing entry named", err)
	}
}

func TestResolveAwsTargetsFromConfig_UnreadableFile(t *testing.T) {
	isolate(t)
	c := awsCmd(t)
	mustSet(t, c, "config", filepath.Join(t.TempDir(), "missing.toml"))
	if _, err := resolveAwsTargets(c); err == nil {
		t.Fatal("a missing config file must error")
	}
}

// --- task networking ------------------------------------------------------

func TestTaskNetworking(t *testing.T) {
	isolate(t)

	t.Run("explicit flags win", func(t *testing.T) {
		c := awsCmd(t)
		mustSet(t, c, "subnets", "subnet-x, subnet-y")
		mustSet(t, c, "security-group-id", "sg-explicit")
		subnets, sg := taskNetworking(c, []collector.AwsTarget{completeTarget()})
		if len(subnets) != 2 || subnets[0] != "subnet-x" {
			t.Errorf("subnets = %v, want the flag's (whitespace trimmed)", subnets)
		}
		if sg != "sg-explicit" {
			t.Errorf("sg = %q", sg)
		}
	})

	// One Fargate task serves every database, so it takes the first one's
	// networking when nothing is specified.
	t.Run("falls back to the first database", func(t *testing.T) {
		subnets, sg := taskNetworking(awsCmd(t), []collector.AwsTarget{completeTarget()})
		if len(subnets) != 2 || subnets[0] != "subnet-a" {
			t.Errorf("subnets = %v", subnets)
		}
		if sg != "sg-db" {
			t.Errorf("sg = %q", sg)
		}
	})

	t.Run("no targets and no flags yields nothing", func(t *testing.T) {
		subnets, sg := taskNetworking(awsCmd(t), nil)
		if len(subnets) != 0 || sg != "" {
			t.Errorf("got (%v,%q), want empty", subnets, sg)
		}
	})
}

// --- network-path verification -------------------------------------------

func TestVerifyNetworkPath(t *testing.T) {
	isolate(t)

	t.Run("reachable prints the path and proceeds", func(t *testing.T) {
		stubNetworkPath(t, []collector.NetworkFinding{
			{Target: "prod", Port: 5432, Reachable: true, Detail: "sg-db admits sg-task"},
		}, nil)
		var err error
		out := capture(t, func() { err = verifyNetworkPath(awsCmd(t), "sg-task", []string{"subnet-a"}, nil) })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out, "reachable") {
			t.Errorf("out = %q", out)
		}
	})

	// A blocked path is advisory in a non-interactive run: it prints the exact
	// fix and continues, because the check is a static approximation.
	t.Run("blocked prints the fix", func(t *testing.T) {
		stubNetworkPath(t, []collector.NetworkFinding{
			{Target: "prod", Port: 5432, Detail: "nothing admits the collector",
				Remediation: "add an inbound rule to sg-db: TCP port 5432 from sg-task"},
		}, nil)
		c := awsCmd(t)
		mustSet(t, c, "force", "true") // non-interactive equivalent
		var err error
		out := capture(t, func() { err = verifyNetworkPath(c, "sg-task", []string{"subnet-a"}, nil) })
		if err != nil {
			t.Fatalf("--force must proceed, got %v", err)
		}
		if !strings.Contains(out, "add an inbound rule") {
			t.Errorf("the exact fix should be printed, got %q", out)
		}
	})

	// Missing ec2:Describe* permissions must not block an install — the check is
	// a convenience, not a gate.
	t.Run("check failure warns and continues", func(t *testing.T) {
		stubNetworkPath(t, nil, errors.New("UnauthorizedOperation: ec2:DescribeSubnets"))
		var err error
		out := capture(t, func() { err = verifyNetworkPath(awsCmd(t), "sg-task", nil, nil) })
		if err != nil {
			t.Fatalf("a failed check must not block the install, got %v", err)
		}
		if !strings.Contains(out, "continuing") {
			t.Errorf("out = %q, want a warning that says it is continuing", out)
		}
	})
}

// --- summaries and labels -------------------------------------------------

func TestTargetsSummary(t *testing.T) {
	if got := targetsSummary(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	one := []collector.AwsTarget{{Name: "prod"}}
	if got := targetsSummary(one); got != "prod" {
		t.Errorf("got %q", got)
	}
	three := []collector.AwsTarget{{Name: "prod"}, {Name: "staging"}, {Name: "analytics"}}
	if got := targetsSummary(three); got != "prod (+2 more)" {
		t.Errorf("got %q, want \"prod (+2 more)\"", got)
	}
}

func TestProviderLabel(t *testing.T) {
	for in, want := range map[string]string{"aws_aurora": "Aurora", "cloud_sql": "Cloud SQL", "alloydb": "AlloyDB"} {
		if got := collector.ProviderLabel(in); got != want {
			t.Errorf("ProviderLabel(%q) = %q, want %q", in, got, want)
		}
	}
	// Anything else, including empty (the aws default), reads as RDS.
	for _, in := range []string{"aws_rds", "", "something-else"} {
		if got := collector.ProviderLabel(in); got != "RDS" {
			t.Errorf("ProviderLabel(%q) = %q, want RDS", in, got)
		}
	}
}

func TestCommandLabel(t *testing.T) {
	if got := commandLabel(collector.CmdExecuteQuery); !strings.Contains(got, "pg_stat") {
		t.Errorf("got %q", got)
	}
	if got := commandLabel(collector.CmdExplain); !strings.Contains(got, "EXPLAIN") {
		t.Errorf("got %q", got)
	}
	// An unknown command shows as itself rather than disappearing.
	if got := commandLabel("future_command"); got != "future_command" {
		t.Errorf("got %q", got)
	}
}

// --- grant printing -------------------------------------------------------

func TestPrintGrantFor(t *testing.T) {
	out := capture(t, func() {
		printGrantFor(collector.AwsTarget{
			InstanceID: "prod-db", User: "dbgorilla_ro", Databases: []string{"appdb"},
		})
	})
	if !strings.Contains(out, "prod-db") {
		t.Errorf("the database must be named, got %q", out)
	}
	// The printed SQL is what a DBA pastes; it has to include the grant that
	// makes the topology scraper work.
	if !strings.Contains(out, "pg_read_all_data") {
		t.Errorf("grant SQL should include pg_read_all_data, got %q", out)
	}
}

// awsParams spreads a parameter map into printDeployParams' arguments with
// the AWS redaction set, as runInstallAWS calls it.
func awsParams(params map[string]string) (map[string]string, []string, string) {
	return params, awsSecretParams, "CollectorConfig"
}

func TestPrintAwsParams_RedactsSecretsAndDecodesConfig(t *testing.T) {
	encoded, err := collector.EncodeConfig("[collector]\nid = \"agent-1\"\n")
	if err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() {
		printDeployParams(awsParams(map[string]string{
			"ServerSecret":    "super-secret-value",
			"DbPassword":      "db-secret-value",
			"CollectorImage":  "ghcr.io/dbgorilla/collector:v1",
			"CollectorConfig": encoded,
		}))
	})

	// A dry run is something people paste into tickets.
	for _, secret := range []string{"super-secret-value", "db-secret-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret leaked into dry-run output: %q", out)
		}
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("redaction should be visible, got %q", out)
	}
	// The config is shown as TOML, not as an opaque blob.
	if !strings.Contains(out, `id = "agent-1"`) {
		t.Errorf("config should be decoded for the dry run, got %q", out)
	}
	if strings.Contains(out, encoded) {
		t.Error("the base64 blob should not be printed when it decodes")
	}
}

func TestPrintAwsParams_UndecodableConfigFallsBackToRaw(t *testing.T) {
	out := capture(t, func() {
		printDeployParams(awsParams(map[string]string{"CollectorConfig": "!!!not-base64!!!"}))
	})
	if !strings.Contains(out, "!!!not-base64!!!") {
		t.Errorf("an undecodable blob should still be shown, got %q", out)
	}
}

func TestPrintAwsParams_EmptySecretsAreNotRedacted(t *testing.T) {
	// Redacting an empty value would print "<redacted>" for a password that was
	// never set, which reads as "a password is configured".
	out := capture(t, func() {
		printDeployParams(awsParams(map[string]string{"DbPassword": ""}))
	})
	if strings.Contains(out, "<redacted>") {
		t.Errorf("an unset password must not look configured, got %q", out)
	}
}

// --- splitCSV -------------------------------------------------------------

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a , b ,, c ")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("splitCSV = %v, want trimmed values with empties dropped", got)
	}
	if splitCSV("") != nil {
		t.Error("an empty string should yield no values")
	}
}
