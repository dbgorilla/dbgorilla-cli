package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// deployTimeout bounds how long we wait for a stack create/update to finish.
// The stack creates an ECS service, and CloudFormation holds that resource in
// CREATE_IN_PROGRESS until the service reaches steady state — a cold image pull
// or a task that restarts once can legitimately take a while, so this is
// generous. Waiting too long only delays the operator; giving up too early used
// to tear down a healthy deploy (see ErrDeployTimeout).
const deployTimeout = 30 * time.Minute

// deleteTimeout bounds a stack delete. Teardown has no steady-state wait, so it
// does not need the create budget.
const deleteTimeout = 15 * time.Minute

// ErrDeployTimeout means we stopped waiting, NOT that the deploy failed — the
// stack is very likely still converging. Callers must not roll back on it: the
// rollback path deprovisions the identity and deletes the stack, which would
// destroy a healthy in-flight deploy. Test with errors.Is.
var ErrDeployTimeout = errors.New("timed out waiting for the stack to finish")

// waitErr classifies a waiter failure. The SDK reports exhaustion as a plain
// "exceeded max wait time" error, and a poll that failed without an API
// response (DNS, a dropped connection) as a non-APIError; neither is a
// terminal stack failure.
func waitErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "exceeded max wait time"):
		return fmt.Errorf("%w: %w", ErrDeployTimeout, err)
	case strings.Contains(msg, "expected err to be of type smithy.APIError"):
		return fmt.Errorf("%w: lost contact while polling the stack: %w", ErrDeployUnknown, err)
	}
	return err
}

// DeployTimeout exposes the create/update wait budget so the command layer can
// name it in the "still deploying" message.
func DeployTimeout() time.Duration { return deployTimeout }

// errNoStackUpdates signals that an UpdateStack found nothing to change. Callers
// interpret it: idempotent deploy → success; upgrade → "already on that image".
var errNoStackUpdates = errors.New("no updates are to be performed")

// Run deploys the collector stack (create, or update in place) and waits for it
// to reach a terminal state.
func (d FargateDeploy) Run() error { return d.deploy(context.Background()) }

// RunQuiet is Run for the spinner path; it returns no captured output (the error
// is self-describing) so the command layer can render its own UI on failure.
func (d FargateDeploy) RunQuiet() (string, error) { return "", d.deploy(context.Background()) }

func (d FargateDeploy) deploy(ctx context.Context) error {
	tmpl, err := resolveTemplate(ctx, d.TemplateURL)
	if err != nil {
		return err
	}
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return err
	}
	client := cloudformation.NewFromConfig(cfg)

	// Dry run: just validate the template shape (no stack, no changeset).
	if d.DryRun {
		in := &cloudformation.ValidateTemplateInput{}
		tmpl.applyValidate(in)
		if _, err := client.ValidateTemplate(ctx, in); err != nil {
			return fmt.Errorf("template validation failed: %w", err)
		}
		return nil
	}

	merged := make(map[string]string, len(d.Params)+len(d.Secrets))
	for k, v := range d.Params {
		merged[k] = v
	}
	for k, v := range d.Secrets {
		merged[k] = v
	}
	params := cfnParams(merged)
	exists, status, err := stackState(ctx, client, d.StackName)
	if err != nil {
		return err
	}
	if exists {
		switch {
		case stackNeedsRecreate(status):
			// A prior failed create (ROLLBACK_COMPLETE) or an abandoned changeset
			// (REVIEW_IN_PROGRESS) leaves a stack that can't be updated — the only
			// recovery is delete-then-create. (The old `aws cloudformation deploy`
			// did this for us; the SDK path must do it explicitly.)
			if err := deleteStackAndWait(ctx, client, d.StackName); err != nil {
				return fmt.Errorf("stack %q was stuck in %s and could not be cleaned up for a fresh create: %w", d.StackName, status, err)
			}
			// fall through to CreateStack below
		case stackInProgress(status):
			return fmt.Errorf("stack %q is %s — another operation is already in progress; wait for it to finish and re-run: %w", d.StackName, status, ErrDeployBusy)
		default:
			// Healthy stack: update in place (a matching re-run is a no-op success).
			if err := updateStack(ctx, client, d.StackName, tmpl, params); err != nil && !errors.Is(err, errNoStackUpdates) {
				return err
			}
			return nil
		}
	}
	create := &cloudformation.CreateStackInput{
		StackName:    aws.String(d.StackName),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters:   params,
	}
	tmpl.applyCreate(create)
	if _, err := client.CreateStack(ctx, create); err != nil {
		return fmt.Errorf("could not create stack %q: %w", d.StackName, err)
	}
	if err := cloudformation.NewStackCreateCompleteWaiter(client).Wait(ctx,
		&cloudformation.DescribeStacksInput{StackName: aws.String(d.StackName)}, deployTimeout); err != nil {
		return stackWaitError(ctx, client, d.StackName, "create", "creating", err)
	}
	return nil
}

// stackWaitError shapes a create/update waiter failure by its classification.
func stackWaitError(ctx context.Context, client *cloudformation.Client, name, verb, progressive string, err error) error {
	err = waitErr(err)
	switch {
	case errors.Is(err, ErrDeployTimeout):
		return fmt.Errorf("stack %q is still %s after %s: %w", name, progressive, deployTimeout, err)
	case errors.Is(err, ErrDeployUnknown):
		return fmt.Errorf("stack %q may still be %s: %w", name, progressive, err)
	}
	return fmt.Errorf("stack %q did not %s cleanly: %w%s", name, verb, err, stackFailureReason(ctx, client, name))
}

// cfnParams renders a param map as CloudFormation parameters (sorted for a
// stable order; CloudFormation is order-insensitive).
func cfnParams(m map[string]string) []cfntypes.Parameter {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]cfntypes.Parameter, 0, len(keys))
	for _, k := range keys {
		out = append(out, cfntypes.Parameter{ParameterKey: aws.String(k), ParameterValue: aws.String(m[k])})
	}
	return out
}

// stackState reports whether the named stack exists and, if so, its status
// (e.g. CREATE_COMPLETE). A non-existent stack is (false, "", nil).
func stackState(ctx context.Context, client *cloudformation.Client, name string) (bool, string, error) {
	out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return false, "", nil
		}
		return false, "", fmt.Errorf("could not describe stack %q: %w", name, err)
	}
	if len(out.Stacks) == 0 {
		return false, "", nil
	}
	return true, string(out.Stacks[0].StackStatus), nil
}

// stackNeedsRecreate reports whether a stack is in a terminal-but-unusable state
// that only a delete-then-create can recover: a rolled-back/failed first create,
// or a changeset-only stack that was never executed.
func stackNeedsRecreate(status string) bool {
	switch status {
	case "ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "CREATE_FAILED", "REVIEW_IN_PROGRESS", "DELETE_FAILED":
		return true
	}
	return false
}

// stackInProgress reports whether an operation is currently running on the stack
// (create/update/delete/rollback), so we neither update nor recreate under it.
func stackInProgress(status string) bool {
	return strings.HasSuffix(status, "_IN_PROGRESS") && status != "REVIEW_IN_PROGRESS"
}

// deleteStackAndWait deletes a stack and blocks until it's gone, so the caller
// can immediately create a fresh one under the same name.
func deleteStackAndWait(ctx context.Context, client *cloudformation.Client, name string) error {
	if _, err := client.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: aws.String(name)}); err != nil {
		return fmt.Errorf("could not delete stack %q: %w", name, err)
	}
	if err := cloudformation.NewStackDeleteCompleteWaiter(client).Wait(ctx,
		&cloudformation.DescribeStacksInput{StackName: aws.String(name)}, deleteTimeout); err != nil {
		return fmt.Errorf("stack %q did not delete cleanly: %w", name, waitErr(err))
	}
	return nil
}

// updateStack updates a stack (with a new template, or the previous one) and
// waits. Returns errNoStackUpdates when nothing changed (no wait in that case).
func updateStack(ctx context.Context, client *cloudformation.Client, name string, tmpl templateRef, params []cfntypes.Parameter) error {
	in := &cloudformation.UpdateStackInput{
		StackName:    aws.String(name),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters:   params,
	}
	tmpl.applyUpdate(in)
	if _, err := client.UpdateStack(ctx, in); err != nil {
		if strings.Contains(err.Error(), "No updates are to be performed") {
			return errNoStackUpdates
		}
		return fmt.Errorf("could not update stack %q: %w", name, err)
	}
	if err := cloudformation.NewStackUpdateCompleteWaiter(client).Wait(ctx,
		&cloudformation.DescribeStacksInput{StackName: aws.String(name)}, deployTimeout); err != nil {
		return stackWaitError(ctx, client, name, "update", "updating", err)
	}
	return nil
}

// stackFailureReason returns a short "reason: …" from the most recent failed
// stack event (best-effort), to make a waiter failure actionable.
func stackFailureReason(ctx context.Context, client *cloudformation.Client, name string) string {
	out, err := client.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: aws.String(name)})
	if err != nil {
		return ""
	}
	for _, e := range out.StackEvents { // newest first
		if strings.Contains(string(e.ResourceStatus), "FAILED") && e.ResourceStatusReason != nil {
			return fmt.Sprintf("\n  reason: %s (%s)", aws.ToString(e.ResourceStatusReason), aws.ToString(e.LogicalResourceId))
		}
	}
	return ""
}

// DeleteStack initiates deletion of the collector's stack (uninstall / rollback).
// region pins the lookup so a changed default region can't orphan it.
func DeleteStack(stackName, region string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	if _, err := cloudformation.NewFromConfig(cfg).DeleteStack(ctx, &cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	}); err != nil {
		return fmt.Errorf("could not delete stack %q: %w", stackName, err)
	}
	return nil
}

// StackStatus returns the stack's status (e.g. CREATE_COMPLETE), or "" (nil
// error) when the stack does not exist.
func StackStatus(stackName, region string) (string, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return "", err
	}
	out, err := cloudformation.NewFromConfig(cfg).DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return "", nil
		}
		return "", fmt.Errorf("could not describe stack %q: %w", stackName, err)
	}
	if len(out.Stacks) == 0 {
		return "", nil
	}
	return string(out.Stacks[0].StackStatus), nil
}

// UpdateComponents updates an existing collector stack in place to monitor a new
// set of databases. Only the two component-bearing parameters change — the
// config's [[component]] blocks and the matching rds-db:connect grants. The
// stack's own config is the source of truth for everything else: the identity
// and endpoints minted at install are read back, not re-derived, so there is no
// re-mint. Declarative: targets is the full desired set, and a no-op returns nil.
//
// dbPassword carries password auth through the update. It cannot be reused from
// the stack: adding a password-auth database, or rotating an existing password,
// both have to reach the DbPassword parameter or the collector is left with an
// unresolved ${DBG_DB_PASSWORD} reference.
func UpdateComponents(stackName, region string, targets []AwsTarget, dbPassword string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	client := cloudformation.NewFromConfig(cfg)

	encoded, err := stackParam(ctx, client, stackName, configParamKey)
	if err != nil {
		return err
	}
	current, err := DecodeConfig(encoded)
	if err != nil {
		return err
	}
	// Strict: this re-renders the stored config, so a key we cannot model would
	// be silently dropped from a running collector rather than preserved.
	conf, err := StrictParseConfig(current)
	if err != nil {
		return fmt.Errorf("could not read the collector config stored on stack %q: %w", stackName, err)
	}
	accountID, err := AwsAccountID()
	if err != nil {
		return err
	}
	conf.Component = nil
	for _, t := range targets {
		conf.Component = append(conf.Component, awsComponent(t, region))
	}
	rendered, err := conf.Render()
	if err != nil {
		return err
	}
	next, err := EncodeConfig(rendered)
	if err != nil {
		return err
	}

	params := make([]cfntypes.Parameter, 0, len(fargateParamKeys))
	for _, k := range fargateParamKeys {
		p := cfntypes.Parameter{ParameterKey: aws.String(k)}
		switch k {
		case configParamKey:
			p.ParameterValue = aws.String(next)
		case rdsConnectParamKey:
			p.ParameterValue = aws.String(strings.Join(rdsConnectParam(targets, region, accountID), ","))
		case "DbPassword":
			// Only overwrite when this update actually carries a password;
			// otherwise keep whatever the install stored.
			if dbPassword == "" {
				p.UsePreviousValue = aws.Bool(true)
				break
			}
			p.ParameterValue = aws.String(dbPassword)
		default:
			p.UsePreviousValue = aws.Bool(true)
		}
		params = append(params, p)
	}
	// Reuse the stack's own template: an update to the monitored set must not
	// also swap the template out from under a running collector.
	if err := updateStack(ctx, client, stackName, templateRef{UsePreviousTemplate: true}, params); err != nil && !errors.Is(err, errNoStackUpdates) {
		return err
	}
	return nil
}

// stackParam reads one parameter value off an existing stack.
func stackParam(ctx context.Context, client *cloudformation.Client, stackName, key string) (string, error) {
	out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return "", fmt.Errorf("could not describe stack %q: %w", stackName, err)
	}
	if len(out.Stacks) == 0 {
		return "", fmt.Errorf("stack %q does not exist. Run: dbg collector install --target aws", stackName)
	}
	for _, p := range out.Stacks[0].Parameters {
		if aws.ToString(p.ParameterKey) == key {
			return aws.ToString(p.ParameterValue), nil
		}
	}
	return "", fmt.Errorf("stack %q has no %s parameter; it predates this CLI version. Re-run: dbg collector install --target aws", stackName, key)
}

// UpgradeImage rolls the collector to a new image, holding every other parameter
// at its previous value against the stack's existing template (which carries the
// monitored databases).
func UpgradeImage(stackName, region, image string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	params := make([]cfntypes.Parameter, 0, len(fargateParamKeys))
	for _, k := range fargateParamKeys {
		if k == "CollectorImage" {
			params = append(params, cfntypes.Parameter{ParameterKey: aws.String(k), ParameterValue: aws.String(image)})
		} else {
			params = append(params, cfntypes.Parameter{ParameterKey: aws.String(k), UsePreviousValue: aws.Bool(true)})
		}
	}
	err = updateStack(ctx, cloudformation.NewFromConfig(cfg), stackName, templateRef{UsePreviousTemplate: true}, params)
	if errors.Is(err, errNoStackUpdates) {
		return fmt.Errorf("already on %s (nothing to upgrade)", image)
	}
	return err
}

// TailLogs prints the collector's CloudWatch logs. follow polls for new events
// until interrupted, mirroring `docker logs -f` for the local target.
func TailLogs(logGroup, region string, follow bool) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	client := cloudwatchlogs.NewFromConfig(cfg)
	// FilterLogEvents' StartTime is inclusive; the cursor suppresses the events
	// it has already printed at that instant (see logCursor).
	cur := newLogCursor(time.Now().Add(-10 * time.Minute).UnixMilli())
	for {
		// FilterLogEvents is paginated. A busy collector easily exceeds one page,
		// and ignoring NextToken silently drops everything past the first.
		pager := cloudwatchlogs.NewFilterLogEventsPaginator(client, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: aws.String(logGroup),
			StartTime:    aws.Int64(cur.newest),
		})
		for pager.HasMorePages() {
			out, err := pager.NextPage(ctx)
			if err != nil {
				return fmt.Errorf("could not read logs from %q: %w", logGroup, err)
			}
			for _, e := range out.Events {
				if !cur.accept(aws.ToString(e.EventId), aws.ToInt64(e.Timestamp)) {
					continue
				}
				fmt.Println(aws.ToString(e.Message))
			}
		}
		if !follow {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
}

// ScaleService sets the collector ECS service's desired task count (0 = stop,
// 1 = start). The cluster and service both carry the stack name.
func ScaleService(stackName, region string, desired int) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	if _, err := ecs.NewFromConfig(cfg).UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:      aws.String(stackName),
		Service:      aws.String(stackName),
		DesiredCount: aws.Int32(int32(desired)),
	}); err != nil {
		return fmt.Errorf("could not scale service %q to %d: %w", stackName, desired, err)
	}
	return nil
}

// RestartService forces a new deployment of the collector task (rolling restart)
// without changing its configuration.
func RestartService(stackName, region string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return err
	}
	if _, err := ecs.NewFromConfig(cfg).UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:            aws.String(stackName),
		Service:            aws.String(stackName),
		ForceNewDeployment: true,
	}); err != nil {
		return fmt.Errorf("could not restart service %q: %w", stackName, err)
	}
	return nil
}
