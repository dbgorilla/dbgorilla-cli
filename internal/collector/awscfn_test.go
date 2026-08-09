package collector

import "testing"

func TestStackStateClassification(t *testing.T) {
	recreate := []string{"ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "CREATE_FAILED", "REVIEW_IN_PROGRESS", "DELETE_FAILED"}
	for _, s := range recreate {
		if !stackNeedsRecreate(s) {
			t.Errorf("%s should need recreate", s)
		}
	}
	healthy := []string{"CREATE_COMPLETE", "UPDATE_COMPLETE", "UPDATE_ROLLBACK_COMPLETE", "IMPORT_COMPLETE"}
	for _, s := range healthy {
		if stackNeedsRecreate(s) {
			t.Errorf("%s should NOT need recreate", s)
		}
		if stackInProgress(s) {
			t.Errorf("%s is terminal, not in-progress", s)
		}
	}
	for _, s := range []string{"CREATE_IN_PROGRESS", "UPDATE_IN_PROGRESS", "DELETE_IN_PROGRESS", "ROLLBACK_IN_PROGRESS"} {
		if !stackInProgress(s) {
			t.Errorf("%s should be in-progress", s)
		}
	}
	// REVIEW_IN_PROGRESS ends in _IN_PROGRESS but is a recreate case, not a
	// live-operation case (a changeset-only stack, no operation running).
	if stackInProgress("REVIEW_IN_PROGRESS") {
		t.Error("REVIEW_IN_PROGRESS should be treated as recreate, not in-progress")
	}
}
