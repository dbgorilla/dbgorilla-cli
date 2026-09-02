package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// Day-2 operations against the deployment's managed instance group and its
// logs. The template names the group (and its instances' base name) after the
// deployment, so these helpers address it by that name — a lookup would add a
// permission requirement for what is a fixed naming contract, exactly like
// LogGroupFor.

const (
	computeBase = "https://compute.googleapis.com/compute/v1"
	loggingBase = "https://logging.googleapis.com/v2"
)

// computeOpTimeout bounds a compute operation (resize, recreate). These finish
// in seconds; the deploy budget would be absurd here.
const computeOpTimeout = 2 * time.Minute

// migPath is the REGIONAL instance group manager the template creates for a
// deployment; by the naming contract its name is the deployment name.
func migPath(project, region, deploymentName string) string {
	return fmt.Sprintf("%s/projects/%s/regions/%s/instanceGroupManagers/%s", computeBase,
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(deploymentName))
}

// ScaleGcpMig sets the group's target size. 0 stops the collector without
// losing its identity or configuration; 1 resumes it — the ScaleService
// analogue.
func ScaleGcpMig(project, region, deploymentName string, size int) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	u := fmt.Sprintf("%s/resize?size=%d", migPath(project, region, deploymentName), size)
	op, err := startGcpOperation(ctx, cfg, http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("could not scale collector group %q to %d: %w", deploymentName, size, err)
	}
	return waitComputeOperation(ctx, cfg, project, region, op.Name)
}

// RestartGcpMig recreates the group's instances — a rolling restart for a
// size-1 group, the RestartService analogue. The recreated instance pulls the
// same image and config; nothing about the deployment changes.
func RestartGcpMig(project, region, deploymentName string) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	instances, err := listManagedInstances(ctx, cfg, project, region, deploymentName)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return fmt.Errorf("collector group %q has no instances to restart — "+
			"is it stopped? Run: dbg collector start", deploymentName)
	}
	u := migPath(project, region, deploymentName) + "/recreateInstances"
	op, err := startGcpOperation(ctx, cfg, http.MethodPost, u, map[string]any{"instances": instances})
	if err != nil {
		return fmt.Errorf("could not restart collector group %q: %w", deploymentName, err)
	}
	return waitComputeOperation(ctx, cfg, project, region, op.Name)
}

func listManagedInstances(ctx context.Context, cfg gcpConfig, project, region, deploymentName string) ([]string, error) {
	var out struct {
		ManagedInstances []struct {
			Instance string `json:"instance"`
		} `json:"managedInstances"`
	}
	// Answers inline — a plain listing behind a POST, not an LRO.
	u := migPath(project, region, deploymentName) + "/listManagedInstances"
	if err := gcpDo(ctx, cfg, http.MethodPost, u, nil, &out); err != nil {
		return nil, fmt.Errorf("could not list instances of collector group %q: %w", deploymentName, err)
	}
	instances := make([]string, 0, len(out.ManagedInstances))
	for _, mi := range out.ManagedInstances {
		if mi.Instance != "" {
			instances = append(instances, mi.Instance)
		}
	}
	sort.Strings(instances)
	return instances, nil
}

// waitComputeOperation drives a regional compute operation to completion via
// its blocking /wait endpoint, which returns when the operation finishes or
// after the server's own ~2 minute cap — so this loop rarely turns twice.
func waitComputeOperation(ctx context.Context, cfg gcpConfig, project, region, opName string) error {
	if opName == "" {
		return nil
	}
	u := fmt.Sprintf("%s/projects/%s/regions/%s/operations/%s/wait", computeBase,
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(opName))
	deadline := time.Now().Add(computeOpTimeout)
	for {
		var out struct {
			Status string `json:"status"`
			Error  *struct {
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			} `json:"error"`
		}
		if err := gcpDo(ctx, cfg, http.MethodPost, u, nil, &out); err != nil {
			return err
		}
		if out.Error != nil && len(out.Error.Errors) > 0 {
			return fmt.Errorf("%s", out.Error.Errors[0].Message)
		}
		if out.Status == "DONE" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("compute operation %s did not finish in time", opName)
		}
	}
}

// TailGcpLogs prints the collector's container logs from Cloud Logging,
// scoped to the deployment's instances — the TailLogs analogue. `follow`
// polls every few seconds until interrupted.
func TailGcpLogs(project, deploymentName string, follow bool) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	// COS containers log under resource.type="gce_instance". The group names
	// its instances "<deployment>-<4 chars>", so anchor on that: a substring
	// match would also pick up a deployment whose name merely starts the same
	// way.
	filter := fmt.Sprintf(
		`resource.type="gce_instance" AND labels."compute.googleapis.com/resource_name"=~"^%s-[a-z0-9]{4}$"`,
		deploymentName)
	cur := newLogCursor(time.Now().Add(-10 * time.Minute).UnixNano())
	for {
		entries, err := listLogEntries(ctx, cfg, project, filter, time.Unix(0, cur.newest).UTC())
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !cur.accept(e.InsertID, e.Timestamp.UnixNano()) {
				continue
			}
			fmt.Printf("%s %s\n", e.Timestamp.Format(time.RFC3339), e.text())
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

type gcpLogEntry struct {
	InsertID    string         `json:"insertId"`
	Timestamp   time.Time      `json:"timestamp"`
	TextPayload string         `json:"textPayload"`
	JSONPayload map[string]any `json:"jsonPayload"`
}

func (e gcpLogEntry) text() string {
	if e.TextPayload != "" {
		return e.TextPayload
	}
	if msg, ok := e.JSONPayload["message"].(string); ok {
		return msg
	}
	b, _ := json.Marshal(e.JSONPayload)
	return string(b)
}

// listLogEntries reads every entry since `since` (inclusive), following the
// page token: entries:list is paginated and a busy collector easily exceeds
// one page — ignoring the token would silently drop everything past the first.
func listLogEntries(ctx context.Context, cfg gcpConfig, project, filter string, since time.Time) ([]gcpLogEntry, error) {
	var out []gcpLogEntry
	body := map[string]any{
		"resourceNames": []string{"projects/" + project},
		"filter":        fmt.Sprintf(`%s AND timestamp>="%s"`, filter, since.Format(time.RFC3339Nano)),
		"orderBy":       "timestamp asc",
		"pageSize":      1000,
	}
	for {
		var page struct {
			gcpPage
			Entries []gcpLogEntry `json:"entries"`
		}
		if err := gcpDo(ctx, cfg, http.MethodPost, loggingBase+"/entries:list", body, &page); err != nil {
			return nil, fmt.Errorf("could not read logs for project %q: %w", project, err)
		}
		out = append(out, page.Entries...)
		if page.NextPageToken == "" {
			return out, nil
		}
		body["pageToken"] = page.NextPageToken
	}
}
