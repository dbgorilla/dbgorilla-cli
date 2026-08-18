package collector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Resolving an image tag to its immutable digest over the registry's own HTTP
// API, with no container runtime involved.
//
// PinnedRef does the same job by shelling out to `docker pull`, which is fine
// where a daemon exists. The AWS target has no daemon to rely on -- the CLI
// only hands a string to CloudFormation -- and leaving the tag unresolved there
// costs twice: ECS re-pulls on every task start, so a collector changes version
// without anyone asking, and the stack parameter never changes, so an upgrade
// is a no-op CloudFormation declines to act on.

// registryHTTPTimeout bounds each call. Resolving a digest is a courtesy on the
// way to a deploy, not something worth hanging an install on.
const registryHTTPTimeout = 15 * time.Second

// registryClient is the HTTP client used for registry calls (seam for tests).
var registryClient = &http.Client{Timeout: registryHTTPTimeout}

// manifestAccept lists the manifest types a modern multi-platform image can
// present. Omitting the index types makes a registry answer with a
// single-platform manifest, whose digest is NOT the one the tag refers to.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ",")

// RemoteDigest resolves an image reference to "<repo>:<tag>@sha256:...".
//
// A reference that already carries a digest is returned unchanged -- it is
// already immutable, and re-resolving it could only introduce disagreement.
func RemoteDigest(ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	repo := ImageRepoOf(ref)
	tag := ImageTagOf(ref)
	if tag == "" {
		return "", fmt.Errorf("image reference %q has no tag to resolve", ref)
	}
	registry, path, ok := strings.Cut(repo, "/")
	if !ok || !strings.Contains(registry, ".") {
		// No registry host means Docker Hub's implicit one, which this CLI
		// never publishes to. Refusing beats guessing at a different API.
		return "", fmt.Errorf("image reference %q does not name a registry host", ref)
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, path, tag)
	digest, err := headDigest(url, "")
	if err == nil {
		return ref + "@" + digest, nil
	}
	// A registry that wants a token says so rather than refusing outright. The
	// collector image is public, so the anonymous pull token is enough.
	var challenge *authChallengeError
	if !asAuthChallenge(err, &challenge) {
		return "", err
	}
	token, terr := pullToken(challenge.realm, challenge.service, path)
	if terr != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", ref, terr)
	}
	digest, err = headDigest(url, token)
	if err != nil {
		return "", err
	}
	return ref + "@" + digest, nil
}

// authChallengeError reports that the registry asked for a token, and where to
// get one.
type authChallengeError struct {
	realm   string
	service string
}

func (e *authChallengeError) Error() string {
	return "registry requires a token from " + e.realm
}

func asAuthChallenge(err error, out **authChallengeError) bool {
	c, ok := err.(*authChallengeError)
	if ok {
		*out = c
	}
	return ok
}

// headDigest asks the registry what a tag currently points at. The answer is
// the Docker-Content-Digest header; the body is not needed and not read.
func headDigest(url, token string) (string, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := registryClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the image registry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		if realm, service, ok := parseAuthChallenge(resp.Header.Get("Www-Authenticate")); ok {
			return "", &authChallengeError{realm: realm, service: service}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned HTTP %d for %s", resp.StatusCode, url)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("registry gave no digest for %s", url)
	}
	return digest, nil
}

// parseAuthChallenge reads realm and service out of a Bearer challenge.
func parseAuthChallenge(header string) (realm, service string, ok bool) {
	rest, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return "", "", false
	}
	for _, part := range strings.Split(rest, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch k {
		case "realm":
			realm = strings.Trim(v, `"`)
		case "service":
			service = strings.Trim(v, `"`)
		}
	}
	return realm, service, realm != ""
}

// pullToken fetches an anonymous pull token. The collector image is published
// publicly, so no credentials are sent and none are needed.
func pullToken(realm, service, repoPath string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, realm, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", "repository:"+repoPath+":pull")
	req.URL.RawQuery = q.Encode()

	resp, err := registryClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the registry's token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("cannot read the registry's token response: %w", err)
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	if body.Token == "" {
		return "", fmt.Errorf("registry returned no token")
	}
	return body.Token, nil
}
