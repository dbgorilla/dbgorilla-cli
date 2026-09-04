package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The gcp deployment: Infrastructure Manager (managed Terraform) actuating the
// published template. The template is never embedded in this binary.

const infraManagerBase = "https://config.googleapis.com/v1"

// Variables rather than constants so tests can shorten the waits.
var (
	gcpDeployTimeout      = 30 * time.Minute
	gcpDeleteTimeout      = 15 * time.Minute
	gcpPollInterval       = 5 * time.Second
	gcpPollRequestTimeout = 30 * time.Second
)

// GcpDeploy describes one Infrastructure Manager deployment of the collector.
type GcpDeploy struct {
	Project        string
	Region         string
	DeploymentName string
	// TemplateSource is the template's gs:// directory.
	TemplateSource string
	// ServiceAccount is the account Infrastructure Manager actuates Terraform
	// as (projects/{p}/serviceAccounts/{email}).
	ServiceAccount string
	// Inputs are the template's non-secret input variables (gcpInputKeys).
	Inputs map[string]string
	// Secrets are the credential inputs (GcpSecretInputKeys), kept apart from
	// Inputs so nothing that prints Inputs can ever print them; the two meet
	// only in the request body.
	Secrets map[string]string
	DryRun  bool
}

// Run deploys (create, or update in place) and waits for a terminal state.
func (d GcpDeploy) Run() error { return d.deploy(context.Background()) }

// GcpDeployTimeout exposes the wait budget so the command layer can name it.
func GcpDeployTimeout() time.Duration { return gcpDeployTimeout }

func gcpDeploymentPath(project, region, name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/deployments/%s",
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(name))
}

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
	for k, v := range d.Secrets {
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
	// Probed on every path so an unreachable template fails before anything
	// is created.
	if err := probeGcpTemplate(ctx, cfg, d.TemplateSource); err != nil {
		return err
	}
	if d.DryRun {
		// Infrastructure Manager has no validate-only call; the probe is the
		// whole dry run.
		return nil
	}

	path := gcpDeploymentPath(d.Project, d.Region, d.DeploymentName)
	existing, err := getGcpDeployment(ctx, cfg, path)
	if err != nil {
		return err
	}
	if existing != nil {
		if gcpDeploymentInProgress(existing.State) {
			return fmt.Errorf("deployment %q is %s — another operation is already in progress; "+
				"wait for it to finish and re-run: %w", d.DeploymentName, existing.State, ErrDeployBusy)
		}
		// Any settled state (ACTIVE, SUSPENDED, FAILED) re-applies in place.
		return d.mutate(ctx, cfg, http.MethodPatch,
			infraManagerBase+"/"+path+"?updateMask=service_account,terraform_blueprint")
	}
	createURL := fmt.Sprintf("%s/projects/%s/locations/%s/deployments?deploymentId=%s",
		infraManagerBase, url.PathEscape(d.Project), url.PathEscape(d.Region),
		url.QueryEscape(d.DeploymentName))
	return d.mutate(ctx, cfg, http.MethodPost, createURL)
}

func (d GcpDeploy) mutate(ctx context.Context, cfg gcpConfig, method, rawURL string) error {
	op, err := startGcpOperation(ctx, cfg, method, rawURL, d.body())
	if err != nil {
		return fmt.Errorf("could not deploy %q: %w", d.DeploymentName, err)
	}
	if err := waitGcpOperation(ctx, cfg, op, gcpDeployTimeout); err != nil {
		switch {
		case errors.Is(err, ErrDeployTimeout):
			return fmt.Errorf("deployment %q is still applying after %s: %w",
				d.DeploymentName, gcpDeployTimeout, err)
		case errors.Is(err, ErrDeployUnknown):
			return fmt.Errorf("deployment %q may still be applying: %w", d.DeploymentName, err)
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

// GcpDeploymentStatus reports the deployment's state, or "" when it does not
// exist.
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

// DeleteGcpDeployment destroys the deployment and everything Terraform
// created, waiting for the delete. A deployment that does not exist is not an
// error.
func DeleteGcpDeployment(project, region, name string) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	op, err := startGcpOperation(ctx, cfg, http.MethodDelete,
		infraManagerBase+"/"+gcpDeploymentPath(project, region, name)+"?force=true&deletePolicy=DELETE", nil)
	if err != nil {
		if errors.Is(err, errGcpNotFound) {
			return nil
		}
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

// getGcpDeployment fetches a deployment; a 404 is (nil, nil).
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

// gcpOperation is the long-running operation envelope mutations return.
type gcpOperation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func startGcpOperation(ctx context.Context, cfg gcpConfig, method, rawURL string, body any) (*gcpOperation, error) {
	var op gcpOperation
	if err := gcpDo(ctx, cfg, method, rawURL, body, &op); err != nil {
		return nil, err
	}
	return &op, nil
}

// waitGcpOperation polls the operation until done, error, or the budget runs
// out (ErrDeployTimeout). A few consecutive poll failures are tolerated; past
// that the outcome is ErrDeployUnknown, since the server converges regardless.
func waitGcpOperation(ctx context.Context, cfg gcpConfig, op *gcpOperation, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	const maxConsecutivePollFailures = 4
	pollFailures := 0
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
		pollCtx, cancelPoll := context.WithTimeout(ctx, gcpPollRequestTimeout)
		err := gcpDo(pollCtx, cfg, http.MethodGet, infraManagerBase+"/"+op.Name, nil, &next)
		cancelPoll()
		if err != nil {
			pollFailures++
			if pollFailures >= maxConsecutivePollFailures {
				return fmt.Errorf("polling operation %s failed %d times in a row: %v: %w",
					op.Name, pollFailures, err, ErrDeployUnknown)
			}
			continue
		}
		pollFailures = 0
		op = &next
	}
}
