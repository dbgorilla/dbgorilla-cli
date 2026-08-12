package collector

import "testing"

// The upgrade path is only as safe as this comparison: a wrong "newer" verdict
// rolls a customer's collector backwards and reports success.

func TestImageRefParts(t *testing.T) {
	cases := []struct {
		ref, repo, tag, digest string
	}{
		{
			"dbgorillapublic.azurecr.io/dbg-collector:0.3.3@sha256:abc",
			"dbgorillapublic.azurecr.io/dbg-collector", "0.3.3", "sha256:abc",
		},
		{
			"dbgorillapublic.azurecr.io/dbg-collector:0.4.0",
			"dbgorillapublic.azurecr.io/dbg-collector", "0.4.0", "",
		},
		// A registry port is a colon that is NOT a tag separator.
		{"localhost:5000/dbg-collector:1.0", "localhost:5000/dbg-collector", "1.0", ""},
		{"localhost:5000/dbg-collector", "localhost:5000/dbg-collector", "", ""},
		// Digest-only: no tag to compare.
		{"dbg-collector@sha256:abc", "dbg-collector", "", "sha256:abc"},
	}
	for _, c := range cases {
		if got := ImageRepoOf(c.ref); got != c.repo {
			t.Errorf("ImageRepoOf(%q) = %q, want %q", c.ref, got, c.repo)
		}
		if got := ImageTagOf(c.ref); got != c.tag {
			t.Errorf("ImageTagOf(%q) = %q, want %q", c.ref, got, c.tag)
		}
		if got := ImageDigestOf(c.ref); got != c.digest {
			t.Errorf("ImageDigestOf(%q) = %q, want %q", c.ref, got, c.digest)
		}
	}
}

func TestCompareImages_Ordering(t *testing.T) {
	const repo = "dbgorillapublic.azurecr.io/dbg-collector"

	cases := []struct {
		name            string
		current, target string
		want            int
	}{
		{"a real upgrade", repo + ":0.3.3", repo + ":0.4.0", 1},
		{"a downgrade", repo + ":0.4.0", repo + ":0.3.3", -1},
		{"the same version", repo + ":0.4.0", repo + ":0.4.0", 0},
		{"patch level counts", repo + ":0.3.3", repo + ":0.3.4", 1},
		{"minor beats patch", repo + ":0.3.9", repo + ":0.4.0", 1},
		{"major beats minor", repo + ":0.9.9", repo + ":1.0.0", 1},
		// Numeric, not lexical: "0.10.0" is newer than "0.9.0" even though it
		// sorts earlier as a string. This is the classic silent-downgrade bug.
		{"double-digit components compare numerically", repo + ":0.9.0", repo + ":0.10.0", 1},
		{"and in reverse", repo + ":0.10.0", repo + ":0.9.0", -1},
		{"missing components read as zero", repo + ":1.2", repo + ":1.2.0", 0},
		{"a leading v is tolerated", repo + ":v1.2.0", repo + ":1.2.0", 0},
		// A pre-release is older than the release it leads to.
		{"pre-release is older than its release", repo + ":1.2.0-rc.1", repo + ":1.2.0", 1},
		{"release is newer than its pre-release", repo + ":1.2.0", repo + ":1.2.0-rc.1", -1},
		{"pre-releases order among themselves", repo + ":1.2.0-rc.1", repo + ":1.2.0-rc.2", 1},
		// Build metadata carries no ordering.
		{"build metadata is ignored", repo + ":1.2.0+build.5", repo + ":1.2.0", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CompareImages(c.current, c.target)
			if !ok {
				t.Fatalf("CompareImages(%q,%q) could not compare; these are both versions of the same image",
					c.current, c.target)
			}
			if got != c.want {
				t.Errorf("CompareImages(%q,%q) = %d, want %d", c.current, c.target, got, c.want)
			}
		})
	}
}

// A digest pin must not hide the version: the shipped default carries both.
func TestCompareImages_DigestPinnedRefsStillCompare(t *testing.T) {
	const repo = "dbgorillapublic.azurecr.io/dbg-collector"
	got, ok := CompareImages(repo+":0.4.0@sha256:newer", repo+":0.3.3@sha256:older")
	if !ok {
		t.Fatal("digest-pinned references should still compare by tag")
	}
	if got != -1 {
		t.Errorf("got %d, want -1 (a downgrade)", got)
	}
}

// An identical reference is the same thing even when the tag is not a version.
func TestCompareImages_IdenticalRefIsEqual(t *testing.T) {
	const ref = "dbgorillapublic.azurecr.io/dbg-collector:latest@sha256:abc"
	got, ok := CompareImages(ref, ref)
	if !ok || got != 0 {
		t.Errorf("got (%d,%v), want (0,true)", got, ok)
	}
}

// Refusing on an unknown comparison would block every legitimate upgrade to a
// custom or locally-built image, so these must report "cannot tell".
func TestCompareImages_UnorderableCases(t *testing.T) {
	const repo = "dbgorillapublic.azurecr.io/dbg-collector"
	cases := []struct{ name, current, target string }{
		{"different repositories", repo + ":0.4.0", "ghcr.io/acme/collector:0.3.0"},
		{"a floating tag", repo + ":latest", repo + ":0.4.0"},
		{"a commit sha as a tag", repo + ":a1b2c3d", repo + ":0.4.0"},
		{"a digest-only current", repo + "@sha256:abc", repo + ":0.4.0"},
		{"a digest-only target", repo + ":0.4.0", repo + "@sha256:abc"},
		{"an empty current (state predates image tracking)", "", repo + ":0.4.0"},
		{"an empty target", repo + ":0.4.0", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := CompareImages(c.current, c.target); ok {
				t.Errorf("CompareImages(%q,%q) claimed an ordering it cannot know",
					c.current, c.target)
			}
		})
	}
}
