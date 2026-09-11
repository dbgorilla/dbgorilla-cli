package cmd

import (
	"fmt"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/dbgorilla/dbgorilla-cli/internal/style"
	"github.com/spf13/cobra"
)

// The pieces every cloud install shares, whatever actuates the deploy.

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
