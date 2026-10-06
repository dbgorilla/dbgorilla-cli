package cmd

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbgorilla/dbgorilla-cli/internal/collector"
	"github.com/spf13/cobra"
)

// testLatestVersion is what the stubbed registry reports as the newest release.
const testLatestVersion = "9.9.9"

// stubLatestRelease replaces the newest-release lookup for one test.
func stubLatestRelease(t *testing.T, version string, err error) {
	t.Helper()
	orig := latestCollectorRelease
	latestCollectorRelease = func() (string, string, error) {
		if err != nil {
			return "", "", err
		}
		return collector.ImageForVersion(version), version, nil
	}
	t.Cleanup(func() { latestCollectorRelease = orig })
}

func imageTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("image", "", "")
	return c
}

func TestResolveImage_NewestReleaseByDefault(t *testing.T) {
	stubLatestRelease(t, "0.12.1", nil)
	img, source, err := resolveImage(imageTestCmd())
	if err != nil {
		t.Fatal(err)
	}
	if want := collector.ImageRepo + ":0.12.1"; img != want {
		t.Errorf("image = %q, want %q", img, want)
	}
	if source != "collector 0.12.1, latest release" {
		t.Errorf("source = %q", source)
	}
}

func TestResolveImage_ExplicitImageWins(t *testing.T) {
	// The registry must not be consulted when the operator named an image.
	stubLatestRelease(t, "", errors.New("registry must not be asked"))
	c := imageTestCmd()
	_ = c.Flags().Set("image", "myregistry/dbg-collector:custom")
	img, source, err := resolveImage(c)
	if err != nil {
		t.Fatal(err)
	}
	if img != "myregistry/dbg-collector:custom" || source != "--image override" {
		t.Errorf("got %q (%s), want the --image value", img, source)
	}
}

func TestResolveImage_EmptyExplicitImageRefused(t *testing.T) {
	c := imageTestCmd()
	_ = c.Flags().Set("image", " ")
	if _, _, err := resolveImage(c); err == nil {
		t.Fatal("an empty --image should be refused, not deployed")
	}
}

func TestResolveImage_LookupFailureIsAnErrorNotAFloatingTag(t *testing.T) {
	stubLatestRelease(t, "", errors.New("cannot reach the image registry"))
	img, _, err := resolveImage(imageTestCmd())
	if err == nil {
		t.Fatalf("want an error, got image %q", img)
	}
	if img != "" {
		t.Errorf("no image should be returned on failure, got %q", img)
	}
	if !strings.Contains(err.Error(), "--image "+collector.ImageRepo+":<version>") {
		t.Errorf("error should say how to pass --image: %v", err)
	}
}

// End to end on the AWS path: the stack gets the newest release, by exact
// version and digest, and the operator is told which one and why.
func TestRunInstallAWS_DeploysTheNewestReleasePinned(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubRemoteDigest(t, nil)
	stubLatestRelease(t, "0.12.1", nil)
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	srv := installServer(t, "agent-aws")
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err != nil {
		t.Fatalf("runInstallAWS: %v\n%s", err, out)
	}
	if want := collector.ImageRepo + ":0.12.1@sha256:testdigest"; deploys.params["CollectorImage"] != want {
		t.Errorf("CollectorImage = %q, want %q", deploys.params["CollectorImage"], want)
	}
	if !strings.Contains(out, "collector 0.12.1, latest release") {
		t.Errorf("output should say which version and why, got: %s", out)
	}
}

// A failed lookup stops the install before anything exists: no identity is
// minted and nothing is deployed, so there is nothing to roll back.
func TestRunInstallAWS_LookupFailureStopsBeforeMinting(t *testing.T) {
	isolate(t)
	writeTokens(t)
	stubAWSOK(t)
	stubLatestRelease(t, "", errors.New("cannot reach the image registry"))
	stubDiscover(t, completeTarget(), nil)
	deploys := stubDeploy(t, nil)
	minted := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0_2/collectors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			minted++
		}
		_, _ = io.WriteString(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := awsCmd(t)
	mustSet(t, c, "api-url", srv.URL)
	mustSet(t, c, "db-instance-id", "prod-db")
	mustSet(t, c, "yes", "true")

	var err error
	out := capture(t, func() { err = runInstallAWS(c) })
	if err == nil || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("err = %v, want the lookup failure with the --image hint\n%s", err, out)
	}
	if minted != 0 {
		t.Errorf("no identity may be minted, got %d", minted)
	}
	if deploys.count != 0 {
		t.Errorf("nothing may be deployed, got %d", deploys.count)
	}
}

// The local dry run previews the version a real install would deploy, not the
// moving tag and not an empty image.
func TestRunInstall_DryRunPreviewsTheNewestRelease(t *testing.T) {
	isolate(t)
	stubLatestRelease(t, "0.12.1", nil)
	c := installTestCmd()
	_ = c.Flags().Set("dry-run", "true")
	_ = c.Flags().Set("db-user", "ro")
	var err error
	out := capture(t, func() { err = runInstall(c, nil) })
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, collector.ImageRepo+":0.12.1") {
		t.Errorf("dry run should show the resolved release, got:\n%s", out)
	}
}
