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
// logs. The template creates a REGIONAL MIG named after the deployment, so
// these helpers address it by convention — a lookup would add a permission
// requirement for what is a fixed naming contract, exactly like LogGroupFor.

const computeBase = "https://compute.googleapis.com/compute/v1"
const loggingBase = "https://logging.googleapis.com/v2"

// GcpMigFor returns the managed instance group the template creates for a
// deployment. Pure: a naming convention shared with the template, pinned by
// the template's own tests when it publishes.
func GcpMigFor(deploymentName string) string { return deploymentName }

func migPath(project, region, mig string) string {
	return fmt.Sprintf("projects/%s/regions/%s/instanceGroupManagers/%s",
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(mig))
}

// ScaleGcpMig sets the group's target size. 0 stops the collector without
// losing its identity or configuration; 1 resumes it — the ScaleService
// analogue.
func ScaleGcpMig(project, region, deploymentName string, size int) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/%s/resize?size=%d", computeBase,
		migPath(project, region, GcpMigFor(deploymentName)), size)
	op, err := gcpMutateJSON(ctx, cfg, http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("could not scale collector group %q to %d: %w",
			GcpMigFor(deploymentName), size, err)
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
		return err
	}
	mig := GcpMigFor(deploymentName)
	instances, err := listManagedInstances(ctx, cfg, project, region, mig)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return fmt.Errorf("collector group %q has no instances to restart — "+
			"is it stopped? Run: dbg collector start", mig)
	}
	u := fmt.Sprintf("%s/%s/recreateInstances", computeBase, migPath(project, region, mig))
	op, err := gcpMutateJSON(ctx, cfg, http.MethodPost, u, map[string]any{"instances": instances})
	if err != nil {
		return fmt.Errorf("could not restart collector group %q: %w", mig, err)
	}
	return waitComputeOperation(ctx, cfg, project, region, op.Name)
}

// GcpMigTargetSize reports the group's target size, for status output.
func GcpMigTargetSize(project, region, deploymentName string) (int, error) {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return 0, err
	}
	var out struct {
		TargetSize int `json:"targetSize"`
	}
	if err := gcpGetJSON(ctx, cfg,
		computeBase+"/"+migPath(project, region, GcpMigFor(deploymentName)), &out); err != nil {
		return 0, err
	}
	return out.TargetSize, nil
}

func listManagedInstances(ctx context.Context, cfg gcpConfig, project, region, mig string) ([]string, error) {
	u := fmt.Sprintf("%s/%s/listManagedInstances", computeBase, migPath(project, region, mig))
	var out struct {
		ManagedInstances []struct {
			Instance string `json:"instance"`
		} `json:"managedInstances"`
	}
	// Answers inline — a plain listing behind a POST, not an LRO.
	if err := gcpPostJSON(ctx, cfg, u, nil, &out); err != nil {
		return nil, fmt.Errorf("could not list instances of collector group %q: %w", mig, err)
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
// its blocking /wait endpoint. These finish in seconds; the deploy budget
// would be absurd here.
func waitComputeOperation(ctx context.Context, cfg gcpConfig, project, region, opName string) error {
	if opName == "" {
		return nil
	}
	u := fmt.Sprintf("%s/projects/%s/regions/%s/operations/%s/wait", computeBase,
		url.PathEscape(project), url.PathEscape(region), url.PathEscape(opName))
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var out struct {
			Status string `json:"status"`
			Error  *struct {
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			} `json:"error"`
		}
		if err := gcpPostJSON(ctx, cfg, u, nil, &out); err != nil {
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
// scoped to the deployment's instances by resource name — the TailLogs
// analogue. `follow` polls every few seconds until interrupted.
func TailGcpLogs(project, deploymentName string, follow bool) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return err
	}
	// COS containers log under resource.type="gce_instance"; the instance name
	// carries the MIG (= deployment) name as its prefix.
	filter := fmt.Sprintf(
		`resource.type="gce_instance" AND labels."compute.googleapis.com/resource_name":"%s"`,
		GcpMigFor(deploymentName))
	cursor := time.Now().Add(-10 * time.Minute).UTC()
	// Only the ids at exactly the newest timestamp need remembering — bounded
	// memory, and no same-millisecond entry is ever skipped (the TailLogs
	// approach; ts+1 cursors drop those).
	seen := map[string]bool{}
	for {
		entries, err := listLogEntries(ctx, cfg, project, filter, cursor)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if seen[e.InsertID] {
				continue
			}
			fmt.Printf("%s %s\n", e.Timestamp.Format(time.RFC3339), e.text())
			if e.Timestamp.After(cursor) {
				cursor = e.Timestamp
				seen = map[string]bool{}
			}
			seen[e.InsertID] = true
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

func listLogEntries(ctx context.Context, cfg gcpConfig, project, filter string, since time.Time) ([]gcpLogEntry, error) {
	body := map[string]any{
		"resourceNames": []string{"projects/" + project},
		"filter":        fmt.Sprintf(`%s AND timestamp>="%s"`, filter, since.Format(time.RFC3339Nano)),
		"orderBy":       "timestamp asc",
		"pageSize":      1000,
	}
	var out struct {
		Entries []gcpLogEntry `json:"entries"`
	}
	if err := gcpPostJSON(ctx, cfg, loggingBase+"/entries:list", body, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}
