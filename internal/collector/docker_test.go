package collector

import "testing"

func TestPinnedRefFrom(t *testing.T) {
	got, err := pinnedRefFrom(
		"dbgorillapublic.azurecr.io/dbg-collector:0.2.0",
		"dbgorillapublic.azurecr.io/dbg-collector@sha256:abc123",
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "dbgorillapublic.azurecr.io/dbg-collector:0.2.0@sha256:abc123"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPinnedRefFrom_missingDigest(t *testing.T) {
	if _, err := pinnedRefFrom("repo:1", "repo-without-a-digest"); err == nil {
		t.Error("expected error when the repo digest has no @sha256 part")
	}
}

func TestPinnedRef_alreadyPinned(t *testing.T) {
	// A ref that already carries a digest is returned verbatim and makes no
	// docker call (so this is safe to run without a daemon).
	ref := "dbgorillapublic.azurecr.io/dbg-collector:0.1.0@sha256:deadbeef"
	got, err := PinnedRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != ref {
		t.Errorf("got %q, want unchanged %q", got, ref)
	}
}
