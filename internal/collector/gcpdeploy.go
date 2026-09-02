package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The gcp deployment: Google Infrastructure Manager (managed Terraform)
// actuating a published template that runs the collector container on a
// single-instance managed instance group. The same philosophy as the aws
// target's CloudFormation path: one published template serves the CLI, a
// direct deploy, and a customer's own security review — never a copy embedded
// in this binary.

// Infrastructure Manager's API host. Deployments are
// projects/{p}/locations/{r}/deployments/{name}; mutations return
// long-running operations polled on the same host.
const infraManagerBase = "https://config.googleapis.com/v1"

// gcpDeployTimeout bounds how long we wait for a deployment create/update.
// Terraform applies a MIG and waits for the instance; a cold image pull can
// legitimately take a while. Giving up too early must not tear anything down —
// see ErrDeployTimeout, whose contract this target shares with aws. Variables
// rather than constants so tests can shorten the wait.
var (
	gcpDeployTimeout = 30 * time.Minute
	gcpDeleteTimeout = 15 * time.Minute
	gcpPollInterval  = 5 * time.Second
)

// GcpDeploy describes one Infrastructure Manager deployment of the collector.
type GcpDeploy struct {
	Project        string
	Region         string
	DeploymentName string
	// TemplateSource is the published template's GCS directory
	// (gs://…/collector/gce/<version>/), HostedGcpTemplateSource unless
	// --template-source overrides it the way --template-url does for aws.
	TemplateSource string
	// ServiceAccount is the account Infrastructure Manager actuates Terraform
	// as (projects/{p}/serviceAccounts/{email} — IM requires one explicitly).
	ServiceAccount string
	// Inputs are the template's input variables — the config blob, secrets,
	// image, network. The template's contract, like fargateParamKeys.
	Inputs map[string]string
	DryRun bool
}

// Run deploys (create, or update in place) and waits for a terminal state.
func (d GcpDeploy) Run() error { return d.deploy(context.Background()) }

// GcpDeployTimeout exposes the wait budget so the command layer can name it.
func GcpDeployTimeout() time.Duration { return gcpDeployTimeout }

func gcpDeploymentPath(project, region, name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/deployments/%s",
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(name))
}

// gcpDeployment is the subset of the Infrastructure Manager Deployment
// resource this package reads.
type gcpDeployment struct {
	Name        string `json:"name"`
	State       string `json:"state"` // CREATING | ACTIVE | UPDATING | DELETING | FAILED | SUSPENDED
	StateDetail string `json:"stateDetail"`
	ErrorLogs   string `json:"errorLogs"`
}

func (d GcpDeploy) body() map[string]any {
	inputs := map[string]any{}
	for k, v := range d.Inputs {
		inputs[k] = map[string]any{"inputValue": v}
	}
	return map[string]any{
		"serviceAccount": d.ServiceAccount,
		"terraformBlueprint": map[string]any{
			"gcsSource":   d.TemplateSource,
			"inputValues": inputs,
		},
	}
}

func (d GcpDeploy) deploy(ctx context.Context) error {
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	// The published template must be reachable before anything is touched:
	// this turns "the deploy failed twenty minutes in" into "your egress or the
	// template address is wrong" upfront, on every path — not only the dry run.
	if err := probeGcpTemplate(ctx, cfg, d.TemplateSource); err != nil {
		return err
	}
	if d.DryRun {
		// Infrastructure Manager has no server-side validate-only call the way
		// CloudFormation does; the probe above is what a dry run can check, and
		// it stops here before any mutation.
		return nil
	}

	path := gcpDeploymentPath(d.Project, d.Region, d.DeploymentName)
	existing, err := getGcpDeployment(ctx, cfg, path)
	if err != nil {
		return err
	}
	if existing != nil {
		if gcpDeploymentInProgress(existing.State) {
			return fmt.Errorf("deployment %q is %s — another operation is already in progress; wait for it to finish and re-run",
				d.DeploymentName, existing.State)
		}
		// Any settled state — ACTIVE, SUSPENDED, or FAILED — updates in place:
		// Infrastructure Manager re-applies Terraform against whatever
		// half-converged, unlike a CloudFormation ROLLBACK_COMPLETE stack that
		// must be recreated.
		return d.mutate(ctx, cfg, http.MethodPatch,
			infraManagerBase+"/"+path+"?updateMask=service_account,terraform_blueprint")
	}
	createURL := fmt.Sprintf("%s/projects/%s/locations/%s/deployments?deploymentId=%s",
		infraManagerBase, url.PathEscape(d.Project), url.PathEscape(d.Region),
		url.QueryEscape(d.DeploymentName))
	return d.mutate(ctx, cfg, http.MethodPost, createURL)
}

// mutate issues the create/update and waits for its operation.
func (d GcpDeploy) mutate(ctx context.Context, cfg gcpConfig, method, rawURL string) error {
	op, err := startGcpOperation(ctx, cfg, method, rawURL, d.body())
	if err != nil {
		return fmt.Errorf("could not deploy %q: %w", d.DeploymentName, err)
	}
	if err := waitGcpOperation(ctx, cfg, op, gcpDeployTimeout); err != nil {
		if errors.Is(err, ErrDeployTimeout) {
			return fmt.Errorf("deployment %q is still applying after %s: %w",
				d.DeploymentName, gcpDeployTimeout, err)
		}
		path := gcpDeploymentPath(d.Project, d.Region, d.DeploymentName)
		return fmt.Errorf("deployment %q did not apply cleanly: %w%s",
			d.DeploymentName, err, gcpDeploymentFailureReason(ctx, cfg, path))
	}
	return nil
}

func gcpDeploymentInProgress(state string) bool {
	switch state {
	case "CREATING", "UPDATING", "DELETING":
		return true
	}
	return false
}

// GcpDeploymentStatus reports the deployment's state (e.g. ACTIVE), or ""
// when it does not exist — the same "empty means gone" contract StackStatus
// has, which is what lets install decide update-vs-fresh.
func GcpDeploymentStatus(project, region, name string) (string, error) {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return "", gcpCredsErr(err)
	}
	dep, err := getGcpDeployment(ctx, cfg, gcpDeploymentPath(project, region, name))
	if err != nil {
		return "", err
	}
	if dep == nil {
		return "", nil
	}
	return dep.State, nil
}

// DeleteGcpDeployment removes the deployment and everything Terraform created,
// waiting for the delete (unlike DeleteStack, whose console shows progress —
// Infrastructure Manager gives the operator nothing to watch). The command
// layer runs it under a spinner so the wait is visible.
func DeleteGcpDeployment(project, region, name string) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	// deletePolicy=DELETE destroys the Terraform-managed resources rather than
	// abandoning them; force=true clears stray revisions.
	op, err := startGcpOperation(ctx, cfg, http.MethodDelete,
		infraManagerBase+"/"+gcpDeploymentPath(project, region, name)+"?force=true&deletePolicy=DELETE", nil)
	if err != nil {
		return fmt.Errorf("could not delete deployment %q: %w", name, err)
	}
	if err := waitGcpOperation(ctx, cfg, op, gcpDeleteTimeout); err != nil {
		return fmt.Errorf("deployment %q did not delete cleanly: %w", name, err)
	}
	return nil
}

// gcpDeploymentFailureReason best-effort appends the deployment's error detail.
func gcpDeploymentFailureReason(ctx context.Context, cfg gcpConfig, path string) string {
	dep, err := getGcpDeployment(ctx, cfg, path)
	if err != nil || dep == nil {
		return ""
	}
	detail := dep.StateDetail
	if detail == "" && dep.ErrorLogs != "" {
		detail = "error logs: " + dep.ErrorLogs
	}
	if detail == "" {
		return ""
	}
	return "\n  reason: " + detail
}

// getGcpDeployment fetches a deployment; a 404 is (nil, nil) — not installed
// is a normal answer, not an error.
func getGcpDeployment(ctx context.Context, cfg gcpConfig, path string) (*gcpDeployment, error) {
	var dep gcpDeployment
	err := gcpDo(ctx, cfg, http.MethodGet, infraManagerBase+"/"+path, nil, &dep)
	if errors.Is(err, errGcpNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &dep, nil
}

// gcpOperation is the LRO envelope mutations return.
type gcpOperation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// startGcpOperation issues a mutation (POST/PATCH/DELETE) that answers with a
// long-running operation.
func startGcpOperation(ctx context.Context, cfg gcpConfig, method, rawURL string, body any) (*gcpOperation, error) {
	var op gcpOperation
	if err := gcpDo(ctx, cfg, method, rawURL, body, &op); err != nil {
		return nil, err
	}
	return &op, nil
}

// waitGcpOperation polls the LRO until done, error, or the budget runs out —
// exhaustion is tagged ErrDeployTimeout so callers keep the "do not roll back
// on a timeout" contract the aws target established.
func waitGcpOperation(ctx context.Context, cfg gcpConfig, op *gcpOperation, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		if op.Done {
			if op.Error != nil {
				return errors.New(op.Error.Message)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: operation %s still running", ErrDeployTimeout, op.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gcpPollInterval):
		}
		var next gcpOperation
		if err := gcpDo(ctx, cfg, http.MethodGet, infraManagerBase+"/"+op.Name, nil, &next); err != nil {
			return err
		}
		op = &next
	}
}
