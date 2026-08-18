package collector

import (
	"strconv"
	"strings"
)

// Image-reference comparison, so an upgrade can tell "newer", "same" and
// "older" apart before it replaces a running collector.
//
// The CLI's fallback image is a constant compiled into the binary. That makes
// `collector upgrade` from an out-of-date CLI a downgrade: it resolves the
// version that CLI shipped with, applies it, and reports success. Without a
// comparison the operator is told the upgrade worked while the collector rolls
// backwards.

// ImageRepoOf returns the repository part of an image reference — everything
// before the tag and digest. "host:5000/ns/img:1.2@sha256:..." -> "host:5000/ns/img".
func ImageRepoOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	// A colon before the last slash is a registry port, not a tag.
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

// ImageTagOf returns the tag part of an image reference, or "" when it carries
// none (a digest-only reference).
func ImageTagOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[colon+1:]
	}
	return ""
}

// ImageDigestOf returns the "sha256:..." digest of a reference, or "".
func ImageDigestOf(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return digest
	}
	return ""
}

// CompareImages orders two image references by version.
//
// It returns -1 when target is older than current, 0 when they are the same
// version, +1 when target is newer, and ok=false when the two cannot be
// meaningfully compared — different repositories, a digest-only reference, or a
// tag that is not a version (e.g. "latest", "edge", a commit sha).
//
// Callers must treat ok=false as "proceed": refusing on an unknown comparison
// would block every legitimate upgrade to a custom or locally-built image.
func CompareImages(current, target string) (cmp int, ok bool) {
	if current == "" || target == "" {
		return 0, false
	}
	// Identical references, digest included, are unambiguously the same thing
	// even when the tag is not a version.
	if current == target {
		return 0, true
	}
	// Same digest is the same image, whatever the tags say. This is what makes
	// a moving tag comparable: "repo:0.5.0@sha256:abc" and "repo:latest@sha256:abc"
	// are one image, so an upgrade to latest that resolves to what is already
	// running is recognised as a no-op rather than tearing down a healthy
	// container to install what it is already running.
	if cd, td := ImageDigestOf(current), ImageDigestOf(target); cd != "" && td != "" {
		if cd == td {
			return 0, true
		}
	}
	if ImageRepoOf(current) != ImageRepoOf(target) {
		return 0, false // a different image entirely; not ours to order
	}
	cv, cok := parseVersion(ImageTagOf(current))
	tv, tok := parseVersion(ImageTagOf(target))
	if !cok || !tok {
		return 0, false
	}
	return compareVersions(cv, tv), true
}

// version is a parsed image tag: its numeric components plus whether it
// carried a pre-release suffix.
type version struct {
	parts []int
	pre   string // "rc.1" from "1.2.0-rc.1"; empty for a release
}

// parseVersion reads a version tag. It tolerates a leading "v" and a
// pre-release suffix, and refuses anything without numeric components (so
// "latest" and a commit sha are reported as unparseable rather than ordered).
func parseVersion(tag string) (version, bool) {
	if tag == "" {
		return version{}, false
	}
	tag = strings.TrimPrefix(tag, "v")
	// Build metadata carries no ordering.
	tag, _, _ = strings.Cut(tag, "+")
	core, pre, _ := strings.Cut(tag, "-")

	fields := strings.Split(core, ".")
	parts := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return version{}, false
		}
		parts = append(parts, n)
	}
	if len(parts) == 0 {
		return version{}, false
	}
	return version{parts: parts, pre: pre}, true
}

// compareVersions orders a against b: -1 if b is older than a, 0 if equal,
// +1 if b is newer. A pre-release sorts below the same release ("1.2.0-rc.1"
// is older than "1.2.0").
func compareVersions(a, b version) int {
	n := max(len(a.parts), len(b.parts))
	for i := range n {
		av, bv := at(a.parts, i), at(b.parts, i)
		switch {
		case bv > av:
			return 1
		case bv < av:
			return -1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "": // a is a release, b is a pre-release of it
		return -1
	case b.pre == "": // b is the release
		return 1
	case b.pre > a.pre:
		return 1
	default:
		return -1
	}
}

// at reads an absent version component as 0, so "1.2" and "1.2.0" compare equal.
func at(parts []int, i int) int {
	if i < len(parts) {
		return parts[i]
	}
	return 0
}
