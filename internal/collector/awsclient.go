package collector

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AWS access uses aws-sdk-go-v2 rather than shelling out to the `aws` CLI, so no
// external binary is required. The SDK resolves credentials from the same chain
// as the CLI — env vars, the shared config/credentials files, and SSO — so the
// customer's own credentials are reused and nothing sensitive passes through
// this tool.

var (
	awsCfgOnce sync.Once
	awsCfg     aws.Config
	awsCfgErr  error
)

// loadAWSConfig resolves the default AWS config once (credential + region
// resolution). A non-empty region overrides the resolved one, so uninstall /
// status can target the same region the install captured.
func loadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	awsCfgOnce.Do(func() {
		awsCfg, awsCfgErr = config.LoadDefaultConfig(ctx)
	})
	if awsCfgErr != nil {
		return aws.Config{}, awsCfgErr
	}
	cfg := awsCfg
	if region != "" {
		cfg.Region = region
	}
	return cfg, nil
}

// AwsAvailable returns nil when AWS credentials resolve and work. Mirrors
// DockerAvailable for the local target; the SDK reuses the caller's own
// credentials, so no keys pass through this tool.
func AwsAvailable() error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("could not load AWS configuration "+
			"(set AWS_PROFILE, or run 'aws configure' / 'aws sso login'): %w", err)
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return fmt.Errorf("AWS credentials aren't working "+
			"(run 'aws sso login', or set AWS_PROFILE): %w", err)
	}
	return nil
}

// AwsIdentity returns the caller's ARN, for display during preflight.
func AwsIdentity() (string, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return "", err
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("aws get-caller-identity failed: %w", err)
	}
	return aws.ToString(out.Arn), nil
}

// AwsAccountID returns the caller's AWS account id. The collector's
// rds-db:connect grants name it explicitly, because they are now a stack
// parameter the CLI computes rather than a CloudFormation intrinsic.
func AwsAccountID() (string, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return "", err
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("aws get-caller-identity failed: %w", err)
	}
	return aws.ToString(out.Account), nil
}

// AwsRegion returns the region the SDK resolved (from AWS_REGION, the active
// profile, etc.). Captured at install time and stored so later uninstall/status
// target the same region.
func AwsRegion() string {
	cfg, err := loadAWSConfig(context.Background(), "")
	if err != nil {
		return ""
	}
	return cfg.Region
}
