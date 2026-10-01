package triage

import "testing"

func TestWithEvidenceAccumulates(t *testing.T) {
	finding := NewFinding(CodeNonZeroExit, SeverityCritical, "container exited non-zero").
		WithEvidence("exitCode", 127).
		WithEvidence("reason", "Error")

	if got := len(finding.Evidence); got != 2 {
		t.Fatalf("len(Evidence) = %d, want 2", got)
	}
	if got := finding.Evidence["exitCode"]; got != 127 {
		t.Errorf("Evidence[exitCode] = %v, want 127", got)
	}
	if got := finding.Evidence["reason"]; got != "Error" {
		t.Errorf("Evidence[reason] = %v, want Error", got)
	}
}

// TestWithEvidenceDoesNotMutateReceiver guards the reason WithEvidence copies
// its map instead of writing in place. Findings get built from shared bases and
// templates; if the copy were dropped, two findings derived from one base would
// silently share an evidence map and overwrite each other's values.
func TestWithEvidenceDoesNotMutateReceiver(t *testing.T) {
	base := NewFinding(CodeProbeFailing, SeverityWarning, "probe failing").
		WithEvidence("container", "api")

	readiness := base.WithEvidence("probe", "readiness")
	liveness := base.WithEvidence("probe", "liveness")

	if _, exists := base.Evidence["probe"]; exists {
		t.Error("base finding was mutated by a derived finding")
	}
	if got := readiness.Evidence["probe"]; got != "readiness" {
		t.Errorf("readiness Evidence[probe] = %v, want readiness", got)
	}
	if got := liveness.Evidence["probe"]; got != "liveness" {
		t.Errorf("liveness Evidence[probe] = %v, want liveness", got)
	}
	// the shared key from the base must survive into both copies.
	if got := liveness.Evidence["container"]; got != "api" {
		t.Errorf("liveness lost inherited evidence: Evidence[container] = %v, want api", got)
	}
}

func TestNewFindingHasNoEvidenceMap(t *testing.T) {
	finding := NewFinding(CodeOOMKilled, SeverityCritical, "container was OOMKilled")
	// Evidence is omitempty, so a nil map keeps it out of the JSON entirely
	// rather than emitting a noisy empty object.
	if finding.Evidence != nil {
		t.Errorf("Evidence = %v, want nil for a finding with no evidence", finding.Evidence)
	}
}
