package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
)

const loginRoleARN = "arn:aws:iam::111122223333:role/OrganizationAccountAccessRole"

// awsConfigEnv points the SDK at a config file holding body, with every other
// credential and region source cleared, and makes profile the active one.
func awsConfigEnv(t *testing.T, body, profile string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	credPath := filepath.Join(dir, "credentials")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credPath)
	t.Setenv("AWS_PROFILE", profile)
	for _, k := range []string{"AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}
}

const loginChainConfig = `[default]
login_session = arn:aws:sts::444455556666:assumed-role/Admin/someone
region = us-east-1

[profile target]
role_arn = ` + loginRoleARN + `
source_profile = default
region = us-west-2
role_session_name = dbg-install
`

func TestSDKRejectsLoginSessionAsSourceProfile(t *testing.T) {
	// The defect loadViaLoginSource stands in for. If this starts passing
	// through, the SDK has learned login_session sources and the fallback can go.
	awsConfigEnv(t, loginChainConfig, "target")
	_, err := config.LoadDefaultConfig(context.Background())
	var roleErr config.SharedConfigAssumeRoleError
	if !errors.As(err, &roleErr) || roleErr.Err != nil {
		t.Fatalf("LoadDefaultConfig error = %v, want SharedConfigAssumeRoleError with no cause", err)
	}
}

func TestLoadViaLoginSourceAssumesTheRole(t *testing.T) {
	awsConfigEnv(t, loginChainConfig, "target")
	_, loadErr := config.LoadDefaultConfig(context.Background())

	cfg, err := loadViaLoginSource(context.Background(), loadErr)
	if err != nil {
		t.Fatalf("loadViaLoginSource: %v", err)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("region = %q, want the active profile's us-west-2", cfg.Region)
	}
	cache, ok := cfg.Credentials.(*aws.CredentialsCache)
	if !ok || !cache.IsCredentialsProvider(&stscreds.AssumeRoleProvider{}) {
		t.Errorf("credentials = %T, want a cached stscreds.AssumeRoleProvider", cfg.Credentials)
	}
}

func TestLoadViaLoginSourceEnvRegionWins(t *testing.T) {
	awsConfigEnv(t, loginChainConfig, "target")
	t.Setenv("AWS_REGION", "eu-west-1")
	_, loadErr := config.LoadDefaultConfig(context.Background())

	cfg, err := loadViaLoginSource(context.Background(), loadErr)
	if err != nil {
		t.Fatalf("loadViaLoginSource: %v", err)
	}
	if cfg.Region != "eu-west-1" {
		t.Errorf("region = %q, want AWS_REGION's eu-west-1", cfg.Region)
	}
}

func TestLoadViaLoginSourceDeclinesWithWorkaround(t *testing.T) {
	// An MFA role needs a token provider this fallback does not carry.
	awsConfigEnv(t, loginChainConfig+"mfa_serial = arn:aws:iam::111122223333:mfa/someone\n", "target")
	_, loadErr := config.LoadDefaultConfig(context.Background())

	_, err := loadViaLoginSource(context.Background(), loadErr)
	if err == nil {
		t.Fatal("loadViaLoginSource succeeded for an mfa_serial role, want the SDK error")
	}
	if !errors.Is(err, loadErr) {
		t.Errorf("error %v does not wrap the SDK's", err)
	}
	if !strings.Contains(err.Error(), "aws configure export-credentials --profile target --format env") {
		t.Errorf("error %q lacks the export-credentials workaround", err)
	}
}

func TestLoadViaLoginSourcePassesOtherErrorsThrough(t *testing.T) {
	other := errors.New("some other failure")
	if _, err := loadViaLoginSource(context.Background(), other); err != other {
		t.Errorf("error = %v, want the original unchanged", err)
	}
}

func TestReadConfigProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	body := `# comment
[default]
region = us-east-1

[profile  target ]
role_arn = arn:aws:iam::1:role/r
s3 =
  max_concurrent_requests = 20
; another comment
Source_Profile = default
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readConfigProfile(path, "target")
	if err != nil {
		t.Fatal(err)
	}
	if got["role_arn"] != "arn:aws:iam::1:role/r" || got["source_profile"] != "default" {
		t.Errorf("target = %v", got)
	}
	if _, nested := got["max_concurrent_requests"]; nested {
		t.Errorf("nested sub-section key leaked into the profile: %v", got)
	}
	if got, _ := readConfigProfile(path, "default"); got["region"] != "us-east-1" {
		t.Errorf("default = %v", got)
	}
	if _, err := readConfigProfile(path, "missing"); err == nil {
		t.Error("missing profile: want an error")
	}
}
