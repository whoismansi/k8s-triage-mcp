package triage

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// ptr is a small helper for the optional int32 fields in workload specs.
func ptr(value int32) *int32 { return &value }

// deployment builds a Deployment with sane defaults so each test only has to
// state what it actually cares about.
func deployment(name string, mutate func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			Generation:        1,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr(3)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			ReadyReplicas:      3,
			AvailableReplicas:  3,
			UpdatedReplicas:    3,
		},
	}
	if mutate != nil {
		mutate(d)
	}
	return d
}

// hasFinding reports whether a finding with the given code is present.
func hasFinding(findings []Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func findingByCode(t *testing.T, findings []Finding, code string) Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Code == code {
			return finding
		}
	}
	t.Fatalf("no finding with code %q in %+v", code, findings)
	return Finding{}
}

func TestGetWorkloadStatusDeployment(t *testing.T) {
	tests := []struct {
		name         string
		deployment   *appsv1.Deployment
		wantHealthy  bool
		wantFindings []string
		wantAbsent   []string
	}{
		{
			name:         "healthy deployment has no findings",
			deployment:   deployment("healthy", nil),
			wantHealthy:  true,
			wantFindings: nil,
			wantAbsent:   []string{CodeReplicasUnavailable, CodeProgressDeadline, CodeRolloutStuck},
		},
		{
			name: "missing replicas reported as critical",
			deployment: deployment("degraded", func(d *appsv1.Deployment) {
				d.Status.ReadyReplicas = 1
				d.Status.AvailableReplicas = 1
			}),
			wantHealthy:  false,
			wantFindings: []string{CodeReplicasUnavailable},
		},
		{
			name: "progress deadline exceeded is distinguished from a generic stall",
			deployment: deployment("stuck", func(d *appsv1.Deployment) {
				d.Status.ReadyReplicas = 0
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type:    appsv1.DeploymentProgressing,
					Status:  "False",
					Reason:  "ProgressDeadlineExceeded",
					Message: `ReplicaSet "stuck-abc123" has timed out progressing.`,
				}}
			}),
			wantHealthy:  false,
			wantFindings: []string{CodeProgressDeadline, CodeReplicasUnavailable},
		},
		{
			name: "non-deadline progressing failure is a warning, not a deadline finding",
			deployment: deployment("paused", func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type:   appsv1.DeploymentProgressing,
					Status: "False",
					Reason: "DeploymentPaused",
				}}
			}),
			wantHealthy:  false,
			wantFindings: []string{CodeRolloutStuck},
			wantAbsent:   []string{CodeProgressDeadline},
		},
		{
			name: "stale observedGeneration is flagged so status is not trusted blindly",
			deployment: deployment("stale", func(d *appsv1.Deployment) {
				d.Generation = 5
				d.Status.ObservedGeneration = 4
			}),
			wantHealthy:  false,
			wantFindings: []string{CodeRolloutStuck},
		},
		{
			name: "scaled to zero is healthy, not degraded",
			deployment: deployment("zero", func(d *appsv1.Deployment) {
				d.Spec.Replicas = ptr(0)
				d.Status.ReadyReplicas = 0
				d.Status.AvailableReplicas = 0
				d.Status.UpdatedReplicas = 0
			}),
			wantHealthy: true,
			wantAbsent:  []string{CodeReplicasUnavailable},
		},
		{
			name: "nil spec.replicas defaults to 1",
			deployment: deployment("defaulted", func(d *appsv1.Deployment) {
				d.Spec.Replicas = nil
				d.Status.ReadyReplicas = 1
				d.Status.AvailableReplicas = 1
				d.Status.UpdatedReplicas = 1
			}),
			wantHealthy: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientset := fake.NewClientset(test.deployment)

			got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
				Namespace: "default",
				Name:      test.deployment.Name,
			})
			if err != nil {
				t.Fatalf("GetWorkloadStatus() error = %v", err)
			}

			if got.Healthy != test.wantHealthy {
				t.Errorf("Healthy = %v, want %v (findings: %+v)", got.Healthy, test.wantHealthy, got.Findings)
			}
			for _, code := range test.wantFindings {
				if !hasFinding(got.Findings, code) {
					t.Errorf("missing expected finding %q; got %+v", code, got.Findings)
				}
			}
			for _, code := range test.wantAbsent {
				if hasFinding(got.Findings, code) {
					t.Errorf("unexpected finding %q; got %+v", code, got.Findings)
				}
			}
			if got.Object.Kind != "Deployment" {
				t.Errorf("Object.Kind = %q, want Deployment", got.Object.Kind)
			}
		})
	}
}

// TestRollingUpdateStuckBehindOldReplicas reproduces the regression scenario
// on a real cluster: the old revision still has every replica ready, while
// the new revision's pod cannot start. Ready equals desired, so without a
// check on updated replicas the deployment would look healthy.
func TestRollingUpdateStuckBehindOldReplicas(t *testing.T) {
	clientset := fake.NewClientset(deployment("orders", func(d *appsv1.Deployment) {
		d.Generation = 2
		d.Status.ObservedGeneration = 2
		d.Status.ReadyReplicas = 2
		d.Status.AvailableReplicas = 2
		d.Status.UpdatedReplicas = 1
		d.Spec.Replicas = ptr(2)
	}))

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "orders",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}
	if got.Healthy {
		t.Error("Healthy = true while a replica is still on the previous revision")
	}
	finding := findingByCode(t, got.Findings, CodeRolloutIncomplete)
	if finding.Evidence["updated"] != int32(1) {
		t.Errorf("Evidence[updated] = %v, want 1", finding.Evidence["updated"])
	}
	if hasFinding(got.Findings, CodeReplicasUnavailable) {
		t.Error("replicas_unavailable raised although every desired replica is ready")
	}
}

// TestFindingsIsNeverNil matters for JSON shape: a nil slice marshals to null,
// which is a different thing for a consumer to handle than an empty array.
func TestFindingsIsNeverNil(t *testing.T) {
	clientset := fake.NewClientset(deployment("healthy", nil))

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "healthy",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}
	if got.Findings == nil {
		t.Error("Findings is nil; it must be an empty slice so it marshals to [] not null")
	}
}

func TestReplicasUnavailableCarriesCounts(t *testing.T) {
	clientset := fake.NewClientset(deployment("degraded", func(d *appsv1.Deployment) {
		d.Status.ReadyReplicas = 1
	}))

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "degraded",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}

	// the evidence map is what the model reasons over, so assert on it directly
	// rather than on the message string.
	finding := findingByCode(t, got.Findings, CodeReplicasUnavailable)
	if got := finding.Evidence["missing"]; got != int32(2) {
		t.Errorf("Evidence[missing] = %v (%T), want int32(2)", got, got)
	}
	if got := finding.Evidence["desired"]; got != int32(3) {
		t.Errorf("Evidence[desired] = %v, want 3", got)
	}
}

func TestGetWorkloadStatusStatefulSet(t *testing.T) {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "default", Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec:   appsv1.StatefulSetSpec{Replicas: ptr(3)},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewClientset(statefulSet)

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "db",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}
	if got.Object.Kind != "StatefulSet" {
		t.Errorf("Object.Kind = %q, want StatefulSet", got.Object.Kind)
	}
	if !hasFinding(got.Findings, CodeReplicasUnavailable) {
		t.Errorf("expected replicas_unavailable; got %+v", got.Findings)
	}
}

// TestGetWorkloadStatusDaemonSet covers the normalisation that matters most:
// DaemonSets have no spec.replicas and report through different status fields.
func TestGetWorkloadStatusDaemonSet(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-agent", Namespace: "kube-system", Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1,
			DesiredNumberScheduled: 3,
			NumberReady:            1,
			NumberAvailable:        1,
			UpdatedNumberScheduled: 3,
		},
	}
	clientset := fake.NewClientset(daemonSet)

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "kube-system", Name: "node-agent",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}
	if got.Replicas.Desired != 3 {
		t.Errorf("Replicas.Desired = %d, want 3 from desiredNumberScheduled", got.Replicas.Desired)
	}
	if got.Replicas.Ready != 1 {
		t.Errorf("Replicas.Ready = %d, want 1 from numberReady", got.Replicas.Ready)
	}
	if got.Healthy {
		t.Error("Healthy = true, want false when only 1 of 3 daemon pods are ready")
	}
}

// TestKindAutoDetection verifies the fallback search finds a workload whose
// kind the caller did not specify.
func TestKindAutoDetection(t *testing.T) {
	clientset := fake.NewClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "only-a-statefulset", Namespace: "default", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	})

	got, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "only-a-statefulset",
	})
	if err != nil {
		t.Fatalf("GetWorkloadStatus() error = %v", err)
	}
	if got.Object.Kind != "StatefulSet" {
		t.Errorf("Object.Kind = %q, want StatefulSet via auto-detection", got.Object.Kind)
	}
}

// TestMissingWorkloadIsAnError pins the error-versus-empty-result contract: an
// absent workload must not look like a healthy one.
func TestMissingWorkloadIsAnError(t *testing.T) {
	clientset := fake.NewClientset()

	_, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "nope",
	})
	if err == nil {
		t.Fatal("GetWorkloadStatus() error = nil, want an error for a workload that does not exist")
	}
}

func TestUnsupportedKindIsRejected(t *testing.T) {
	clientset := fake.NewClientset()

	_, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{
		Namespace: "default", Name: "x", Kind: "CronJob",
	})
	if err == nil {
		t.Fatal("GetWorkloadStatus() error = nil, want an error for an unsupported kind")
	}
}
