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

// The cloud deploy targets (aws, gcp) share this install spine: prior-install
// check, identity preflight, capability gate, identity mint, image pin, state
// saved before the slow deploy, and rollback on failure but never on a timeout.

// dbPasswordFlag resolves --db-password from the flag or the env var. The cloud
// targets never prompt for it.
func dbPasswordFlag(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("db-password"); p != "" {
		return p
	}
	return os.Getenv(collector.DBPasswordEnv)
}

// requireInstallSession is the login gate every install starts with.
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

// requireNoInstall refuses when a collector is already recorded on this
// machine.
func requireNoInstall() error {
	if st, _ := collector.LoadState(); st != nil {
		return fmt.Errorf("a collector is already installed (agent %s). Run `dbg collector uninstall` first, or `dbg collector status`",
			st.AgentID)
	}
	return nil
}

// requireCollectorSupport builds the API client and confirms the deployment
// can provision collectors.
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
// A collector of another target blocks. One of this target whose runtime still
// exists is returned with its status. One whose runtime is gone is announced
// and the install proceeds fresh. Dry runs always take the fresh path.
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
			"   Collector %s is still provisioned in DBGorilla; remove it from the console (this install replaces the local record).",
		noun, name(st), st.AgentID)))
	return nil, "", nil
}

// requireNoRuntime refuses a fresh install when the runtime it would create
// already exists but this machine has no record of it: updating it in place
// would hand it a new identity and orphan the old one.
func requireNoRuntime(dryRun bool, status func() (string, error), noun, name, nameFlag string) error {
	if dryRun {
		return nil
	}
	s, err := status()
	if err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	return fmt.Errorf("%s %q already exists (%s) but this machine has no record of it. "+
		"Run `dbg collector uninstall` where it was installed, delete it from the console, or pass %s to use another name",
		noun, name, s, nameFlag)
}

// printCloudIdentity runs the credential preflight and names the principal
// the install will act as.
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
// API. When that fails the tag deploys as-is, with a warning. restartNoun
// names the runtime unit ("task", "instance").
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

// saveStateOrWarn records the install before the slow deploy, so an
// interrupted install leaves a collector that status/uninstall can find.
func saveStateOrWarn(st *collector.State) {
	if err := collector.SaveState(st); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not save local state: %v", err)))
	}
}

// deprovisionOrWarn deletes a freshly minted identity that will not be used.
func deprovisionOrWarn(client *api.Client, agentID string) {
	if err := client.DeleteCollector(agentID); err != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  could not deprovision %s: %v (remove it from the console)", agentID, err)))
	}
}

// cloudDeployFailed handles a deploy error and reports whether the runtime was
// left in place. A timeout, an unobserved outcome, or an interrupt leaves
// everything (runtime, identity, state) for `status` to pick up. A busy
// refusal created nothing, so only this run's identity is deprovisioned.
// Anything else rolls back both the identity and the runtime.
func cloudDeployFailed(err error, client *api.Client, agentID string, budget time.Duration,
	noun, name string, deleteRuntime func() error, watchHint string,
) (kept bool, result error) {
	switch {
	case errors.Is(err, collector.ErrDeployTimeout):
		fmt.Println(style.Warn(fmt.Sprintf("⚠  Still deploying after %s. The %s was NOT rolled back — "+
			"it is most likely still converging.", budget, noun)))
		fmt.Print(watchHint)
		return true, nil
	case errors.Is(err, errInterrupted):
		fmt.Println(style.Warn(fmt.Sprintf("⚠  Interrupted while the %s was converging — nothing was rolled back.", noun)))
		return true, fmt.Errorf("%w\n\nRun `dbg collector status` to see whether the %s converged; "+
			"to remove it, run `dbg collector uninstall`", err, noun)
	case errors.Is(err, collector.ErrDeployUnknown):
		fmt.Println(style.Warn(fmt.Sprintf("⚠  Lost sight of the %s while it was converging — nothing was rolled back.", noun)))
		return true, fmt.Errorf("%w\n\nWhen connectivity returns, run `dbg collector status`: "+
			"if the %s converged, the install is complete; if it failed, run `dbg collector uninstall` and re-run the install", err, noun)
	case errors.Is(err, collector.ErrDeployBusy):
		fmt.Printf("Nothing to roll back on the %s; deprovisioning this run's identity...\n", noun)
		deprovisionOrWarn(client, agentID)
		_ = collector.RemoveState()
		return false, fmt.Errorf("%w\n\nWait for it to finish, then re-run", err)
	}
	fmt.Printf("Deploy failed; rolling back the provisioned identity and %s...\n", noun)
	deprovisionOrWarn(client, agentID)
	if serr := deleteRuntime(); serr != nil {
		fmt.Println(style.Warn(fmt.Sprintf("⚠  %v (delete %s %s from the console)", serr, noun, name)))
	}
	_ = collector.RemoveState()
	return false, fmt.Errorf("%w\n\nRolled back. Fix the issue above and re-run", err)
}

// cloudStatus prints a cloud collector's record, its runtime's state, and its
// control-plane connection. A status error is reported, not fatal.
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

// printDeployParams prints a deploy's rendered parameters for a dry run:
// sorted, secrets redacted when set, the config decoded to readable TOML.
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
// collector.ResolveCommands, supplying the interactive checklist on a real
// terminal. label names a database in the checklist's title.
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
