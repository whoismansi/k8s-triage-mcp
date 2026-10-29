package triage

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

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

// TestEmptyIdentifiersAreRejected guards against namespace "" becoming a
// cluster-wide query. every tool must refuse an empty namespace or name before
// it reaches the API, and say which field was empty.
func TestEmptyIdentifiersAreRejected(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()

	tests := []struct {
		name  string
		field string
		call  func() error
	}{
		{"status empty namespace", "namespace", func() error {
			_, err := GetWorkloadStatus(ctx, clientset, WorkloadStatusInput{Name: "web"})
			return err
		}},
		{"status empty name", "name", func() error {
			_, err := GetWorkloadStatus(ctx, clientset, WorkloadStatusInput{Namespace: "default"})
			return err
		}},
		{"crashloop empty namespace", "namespace", func() error {
			_, err := ExplainCrashloop(ctx, clientset, CrashloopInput{Pod: "p"})
			return err
		}},
		{"crashloop empty pod", "pod", func() error {
			_, err := ExplainCrashloop(ctx, clientset, CrashloopInput{Namespace: "default"})
			return err
		}},
		{"events empty namespace with selector", "namespace", func() error {
			_, err := GetPodEvents(ctx, clientset, PodEventsInput{LabelSelector: "app=web"})
			return err
		}},
		{"events empty namespace with pod", "namespace", func() error {
			_, err := GetPodEvents(ctx, clientset, PodEventsInput{Pod: "p"})
			return err
		}},
		{"diff empty namespace", "namespace", func() error {
			_, err := DiffDeploymentRevisions(ctx, clientset, RevisionDiffInput{Name: "web"})
			return err
		}},
		{"diff empty name", "name", func() error {
			_, err := DiffDeploymentRevisions(ctx, clientset, RevisionDiffInput{Namespace: "default"})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil {
				t.Fatal("error = nil, want a validation error")
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Errorf("error %q does not name the empty field %q", err, test.field)
			}
		})
	}
}

// TestEmptyNamespaceNeverReachesTheAPI checks the guard sits before any call:
// with namespace "" the fake clientset must record no list or get at all.
func TestEmptyNamespaceNeverReachesTheAPI(t *testing.T) {
	clientset := fake.NewClientset()
	_, _ = GetPodEvents(context.Background(), clientset, PodEventsInput{LabelSelector: "app=web"})
	if actions := clientset.Actions(); len(actions) != 0 {
		t.Errorf("API was called %d time(s) with an empty namespace: %v", len(actions), actions)
	}
}
