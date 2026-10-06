package collector

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// aws-sdk-go-v2/config (v1.33.2, and still v1.33.6) resolves a profile written
// by `aws login` (`login_session = <arn>`) when it is the active profile, but
// not when it is the source_profile of a role profile: the source check
// (SharedConfig.hasCredentials) does not count login_session, so the load fails
// with SharedConfigAssumeRoleError and no cause ("failed to load assume role
// <arn>, of profile <source>, <nil>"). The `aws` CLI accepts the same file.
//
// loadViaLoginSource does what the SDK would: load the source profile on its
// own, which resolves its login credentials, and assume the role on top of
// them. It handles one link — the active profile sourcing a login_session
// profile directly — and nothing else, so any other configuration keeps the
// SDK's own error, with the workaround added.
func loadViaLoginSource(ctx context.Context, loadErr error) (aws.Config, error) {
	var roleErr config.SharedConfigAssumeRoleError
	if !errors.As(loadErr, &roleErr) || roleErr.Err != nil {
		return aws.Config{}, loadErr
	}

	env, err := config.NewEnvConfig()
	if err != nil {
		return aws.Config{}, loadErr
	}
	active := env.SharedConfigProfile
	if active == "" {
		active = config.DefaultSharedConfigProfile
	}
	file := env.SharedConfigFile
	if file == "" {
		file = config.DefaultSharedConfigFilename()
	}
	target, err := readConfigProfile(file, active)
	if err != nil {
		return aws.Config{}, loginWorkaround(loadErr, active)
	}
	source, err := readConfigProfile(file, roleErr.Profile)
	if err != nil || source["login_session"] == "" ||
		target["source_profile"] != roleErr.Profile || target["role_arn"] != roleErr.RoleARN ||
		target["mfa_serial"] != "" {
		return aws.Config{}, loginWorkaround(loadErr, active)
	}

	srcCfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(roleErr.Profile))
	if err != nil {
		return aws.Config{}, loginWorkaround(fmt.Errorf("%w (source profile: %w)", loadErr, err), active)
	}
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(srcCfg), roleErr.RoleARN,
		func(o *stscreds.AssumeRoleOptions) {
			if v := target["role_session_name"]; v != "" {
				o.RoleSessionName = v
			}
			if v := target["external_id"]; v != "" {
				o.ExternalID = aws.String(v)
			}
			if v, err := strconv.Atoi(target["duration_seconds"]); err == nil && v > 0 {
				o.Duration = time.Duration(v) * time.Second
			}
		})

	cfg := srcCfg.Copy()
	cfg.Credentials = aws.NewCredentialsCache(provider)
	// The environment's region wins over any profile's, as in the SDK; past
	// that, the region is the active profile's, not the source's.
	if env.Region == "" && target["region"] != "" {
		cfg.Region = target["region"]
	}
	return cfg, nil
}

// loginWorkaround adds to an AWS config error the way around it that always
// works: hand this tool short-lived credentials exported by the `aws` CLI.
func loginWorkaround(err error, profile string) error {
	return fmt.Errorf("%w\n\nIf this profile's credentials come from 'aws login', export them for this tool:\n"+
		"  eval \"$(aws configure export-credentials --profile %s --format env)\"\n"+
		"  unset AWS_PROFILE", err, profile)
}

// readConfigProfile returns the top-level keys of one profile in an AWS
// config file: `[default]`, or `[profile <name>]` for any other name.
// Indented lines (the nested sub-sections the format allows) are skipped.
func readConfigProfile(path, name string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	want := "profile " + name
	found := false
	keys := map[string]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.Join(strings.Fields(line[1:len(line)-1]), " ")
			in = section == want || (name == config.DefaultSharedConfigProfile && section == name)
			found = found || in
			continue
		}
		if !in || raw[0] == ' ' || raw[0] == '\t' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			keys[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("profile %q not found in %s", name, path)
	}
	return keys, nil
}
