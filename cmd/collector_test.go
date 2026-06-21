package cmd

import (
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/api"
	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

func imageTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("image", collector.DefaultImage, "")
	return c
}

func TestResolveImage_DefaultWhenNoPreferredOrFlag(t *testing.T) {
	img, src := resolveImage(imageTestCmd(), &api.CollectorCredentials{})
	if img != collector.DefaultImage {
		t.Errorf("got %q, want default %q", img, collector.DefaultImage)
	}
	if src != "CLI default" {
		t.Errorf("source = %q, want CLI default", src)
	}
}

func TestResolveImage_PreferredVersionWins(t *testing.T) {
	img, src := resolveImage(imageTestCmd(), &api.CollectorCredentials{PreferredCollectorVersion: "0.2.0"})
	if want := collector.ImageForVersion("0.2.0"); img != want {
		t.Errorf("got %q, want %q", img, want)
	}
	if src == "CLI default" {
		t.Errorf("source should reflect the deployment-blessed version, got %q", src)
	}
}

func TestResolveImage_ExplicitFlagOverridesPreferred(t *testing.T) {
	c := imageTestCmd()
	_ = c.Flags().Set("image", "myregistry/dbg-collector:custom") // marks the flag changed
	img, src := resolveImage(c, &api.CollectorCredentials{PreferredCollectorVersion: "0.2.0"})
	if img != "myregistry/dbg-collector:custom" {
		t.Errorf("explicit --image should win, got %q", img)
	}
	if src != "--image override" {
		t.Errorf("source = %q, want --image override", src)
	}
}
