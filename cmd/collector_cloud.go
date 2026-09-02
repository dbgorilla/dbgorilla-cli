package cmd

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// The cloud deploy targets (aws, gcp) share one install spine: check for a
// prior install, name the identity that will act, confirm the deployment
// supports collectors, mint an identity, pin the image, save state before the
// slow deploy, and on failure roll back — but never on a timeout. These are
// the target-neutral pieces; each target's own file adds its discovery, auth,
// networking and deployment API. A third cloud extends this file rather than
// copying an install function.

// dbPasswordFlag resolves --db-password from the flag or the env var. The cloud
// targets never prompt for it: the value rides into the deployment's secret
// store, so it must be supplied deliberately.
func dbPasswordFlag(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("db-password"); p != "" {
		return p
	}
	return os.Getenv(collector.DBPasswordEnv)
}

// requireInstallSession is the login gate every install starts with: a
// resolvable API URL and a current login.
func requireInstallSession(cmd *cobra.Command) (apiURL string, err error) {
	apiURL, err = requireAPIURL(cmd)
	if err != nil {
		return "", err
	}
	if _, err := requireLogin(); err != nil {
		return "", err
	}
	return apiURL, nil
}

// requireCollectorSupport builds the API client and confirms the deployment
// can provision collectors at all, before anything cloud-side is touched.
func requireCollectorSupport(cmd *cobra.Command, apiURL string) (*api.Client, error) {
	client := newAPIClient(cmd)
	supported, err := client.CollectorSupported()
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", apiURL, err)
	}
	if !supported {
		return nil, api.ErrCollectorUnsupported
	}
	return client, nil
}

// priorCloudInstall inspects the local install record before a cloud install.
// A collector of another target blocks (it cannot be reconciled here). One of
// this target whose runtime still exists is returned with its status, for the
// caller to update or refuse. One whose runtime is gone — deleted from the
// console, or left by an uninstall that could not deprovision the identity —
// is announced, and the install proceeds fresh; updating it would fail deep in
// the cloud API instead. Dry runs always take the fresh path.
func priorCloudInstall(dryRun bool, isMine func(*collector.State) bool,
	status func(*collector.State) (string, error), noun string, name func(*collector.State) string,
) (*collector.State, string, error) {
	st, _ := collector.LoadState()
	if st == nil || dryRun {
		return nil, "", nil
	}
	if !isMine(st) {
		return nil, "", fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
			st.AgentID)
	}
	s, err := status(st)
	if err != nil {
		return nil, "", err
	}
	if s != "" {
		return st, s, nil
	}
	fmt.Println(style.Warn(fmt.Sprintf(
		"⚠  %s %q from the last install no longer exists — installing fresh.\n"+
			"   Collector %s is still provisioned in DBGorilla; remove it with `dbg collector uninstall` once this finishes.",
		noun, name(st), st.AgentID)))
	return nil, "", nil
}

// printCloudIdentity runs the credential preflight and names the principal
// the install will act as — before it acts. The caller's own credentials are
// reused; no keys pass through this tool.
func printCloudIdentity(cloud string, available func() error, identity func() (string, error)) error {
	if err := available(); err != nil {
		return err
	}
	id, err := identity()
	if err != nil {
		return err
	}
	fmt.Println(style.Success(fmt.Sprintf("✓ %s identity: %s", cloud, id)))
	return nil
}

// pinImageOrWarn resolves the image tag to a digest over the registry's HTTP
// API (the cloud installers may have no container runtime), so the deployment
// records what actually runs and an upgrade has something to compare against.
// When that fails the tag deploys as-is, with a warning: the runtime re-pulls
// whenever it restarts the collector, so its version may then change on a
// restart nobody asked for. restartNoun names that runtime unit ("task",
// "instance").
func pinImageOrWarn(image, restartNoun string) string {
	pinned, err := pinImageRemote(image)
	if err == nil {
		return pinned
	}
	fmt.Println(style.Warn(fmt.Sprintf(
		"⚠  could not resolve %s to a fixed version (%v).\n"+
			"   Deploying the tag as-is: the collector may change version when its %s restarts.",
		image, err, restartNoun)))
	return image
}

// saveStateOrWarn records the install. It runs BEFORE the slow deploy, so an
// interrupted install leaves a tracked collector that status/uninstall can find
// and clean — not an orphaned runtime plus identity.
func saveStateOrWarn(st *collector.State) {
	if err := collector.SaveState(st); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not save local state: %v", err)))
	}
}

// cloudDeployFailed handles a deploy error. A timeout is not a failure — the
// runtime is still converging, so it is left alone and the operator is handed
// the way to watch it (watchHint, printed verbatim). Anything else rolls back
// BOTH the identity and the runtime: leaving either behind means a customer
// pays for an orphan they cannot see.
func cloudDeployFailed(err error, client *api.Client, agentID string, budget time.Duration,
	noun, name string, deleteRuntime func() error, watchHint string,
) error {
	if errors.Is(err, collector.ErrDeployTimeout) {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  Still deploying after %s. The %s was NOT rolled back — "+
			"it is most likely still converging.", budget, noun)))
		fmt.Print(watchHint)
		return nil
	}
	fmt.Printf("Deploy failed; rolling back the provisioned identity and %s...\n", noun)
	if derr := client.DeleteCollector(agentID); derr != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not auto-deprovision %s: %v (remove it from the console)", agentID, derr)))
	}
	if serr := deleteRuntime(); serr != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not delete %s %s: %v (delete it from the console)", noun, name, serr)))
	}
	_ = collector.RemoveState()
	return fmt.Errorf("%w\n\nRolled back. Fix the issue above and re-run", err)
}

// cloudStatus prints a cloud collector's record, its runtime's state, and its
// control-plane connection. resource labels the runtime unit ("Stack",
// "Deployment"); a status error is reported, not fatal — the record is still
// worth showing.
func cloudStatus(cmd *cobra.Command, st *collector.State, resource, name string, status func() (string, error)) error {
	fmt.Printf("Agent:      %s\n", st.AgentID)
	fmt.Printf("Tenant:     %s\n", st.TenantID)
	fmt.Printf("Target:     %s — %s\n", st.Target, st.TargetName)
	fmt.Printf("Image:      %s\n", st.Image)
	fmt.Printf("%-11s %s\n", resource+":", name)
	s, err := status()
	switch {
	case err != nil:
		fmt.Println(style.Warn(fmt.Sprintf("Deploy:     status unknown (%v)", err)))
	case s == "":
		fmt.Println(style.Warn(fmt.Sprintf("Deploy:     %s not found", strings.ToLower(resource))))
	default:
		fmt.Println(style.Success(fmt.Sprintf("Deploy:     %s", s)))
	}
	printConnectionStatus(cmd, st.AgentID)
	return nil
}

// printDeployParams prints a deploy's rendered parameters for a dry run —
// something people paste into tickets. Sorted; secrets redacted, but only when
// set (an unset password must not read as configured); the config decoded back
// to readable TOML, or shown raw when it will not decode.
func printDeployParams(params map[string]string, secretKeys []string, configKey string) {
	secret := map[string]bool{}
	for _, k := range secretKeys {
		secret[k] = true
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := params[k]
		switch {
		case secret[k]:
			if v != "" {
				v = "<redacted>"
			}
		case k == configKey:
			if decoded, err := collector.DecodeConfig(v); err == nil {
				fmt.Printf("    %s =\n", k)
				for _, line := range strings.Split(strings.TrimRight(decoded, "\n"), "\n") {
					fmt.Printf("      %s\n", line)
				}
				continue
			}
		}
		fmt.Printf("    %s = %s\n", k, v)
	}
}

// resolveCommands reads the query-analysis flags and hands the precedence to
// collector.ResolveCommands, supplying the interactive checklist when this is a
// real terminal. The policy lives there; this is the flag-and-prompt half.
// label names a database in the checklist's title.
func resolveCommands[T any, PT interface {
	*T
	collector.CommandTarget
}](cmd *cobra.Command, targets []T, label func(T) string) bool {
	req := collector.CommandRequest{
		ForcedOff: commandsForcedOff(cmd),
		Explicit:  cmd.Flags().Changed("commands"),
	}
	if req.Explicit {
		v, _ := cmd.Flags().GetString("commands")
		req.Commands = splitCSV(v)
	}
	var prompt func(T) []string
	if interactiveSelectable(cmd) && !req.Explicit {
		prompt = func(t T) []string { return promptCommands(PT(&t).CommandEngine(), label(t)) }
	}
	return collector.ResolveCommands[T, PT](targets, req, prompt)
}

func awsTargetLabel(t collector.AwsTarget) string { return orUnknown(t.Name) }
func gcpTargetLabel(t collector.GcpTarget) string { return t.DisplayName() }
