package triage

import (
	"context"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

const deploymentUID = types.UID("deployment-uid")

var appLabels = map[string]string{"app": "api"}

func revisionDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: deploymentUID},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: appLabels}},
	}
}

// replicaSetFor builds a ReplicaSet owned by the api deployment at a revision.
func replicaSetFor(revision int64, spec corev1.PodSpec) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "api-rev" + strconv.FormatInt(revision, 10),
			Namespace:   "default",
			Labels:      appLabels,
			Annotations: map[string]string{revisionAnnotation: strconv.FormatInt(revision, 10)},
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: "api", UID: deploymentUID,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: spec}},
	}
}

// baseSpec is a realistic healthy template; tests mutate a copy of it.
func baseSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "api",
			Image: "registry.example/api:1.4.0",
			Env: []corev1.EnvVar{
				{Name: "LOG_LEVEL", Value: "info"},
				{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url",
					},
				}},
			},
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
				InitialDelaySeconds: 10,
			},
		}},
	}
}

func diffRevisions(t *testing.T, objects []runtime.Object, in RevisionDiffInput) RevisionDiffOutput {
	t.Helper()
	out, err := DiffDeploymentRevisions(context.Background(), fake.NewClientset(objects...), in)
	if err != nil {
		t.Fatalf("DiffDeploymentRevisions() error = %v", err)
	}
	return out
}

func changeFor(t *testing.T, changes []SpecChange, field string) SpecChange {
	t.Helper()
	for _, change := range changes {
		if change.Field == field {
			return change
		}
	}
	t.Fatalf("no change for field %q in %+v", field, changes)
	return SpecChange{}
}

// TestRegressionBetweenRevisions is the headline scenario: v2 changed the
// image and broke an env var reference, and the diff says exactly that.
func TestRegressionBetweenRevisions(t *testing.T) {
	broken := baseSpec()
	broken.Containers[0].Image = "registry.example/api:1.5.0"
	broken.Containers[0].Env[1].ValueFrom.SecretKeyRef.Key = "connection-string"

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, broken),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	if out.From.Revision != 1 || out.To.Revision != 2 {
		t.Errorf("compared %d -> %d, want 1 -> 2 by default", out.From.Revision, out.To.Revision)
	}

	image := changeFor(t, out.Changes, "image")
	if image.From != "registry.example/api:1.4.0" || image.To != "registry.example/api:1.5.0" {
		t.Errorf("image change = %+v", image)
	}

	env := changeFor(t, out.Changes, "env.DATABASE_URL")
	if env.From != "secretKeyRef(db/url)" || env.To != "secretKeyRef(db/connection-string)" {
		t.Errorf("env change = %+v, want the secret key reference to change", env)
	}
	if !hasFinding(out.Findings, CodeImageChanged) || !hasFinding(out.Findings, CodeEnvChanged) {
		t.Errorf("findings = %+v, want image_changed and env_changed", out.Findings)
	}
	if len(out.Changes) != 2 {
		t.Errorf("len(Changes) = %d, want exactly 2; got %+v", len(out.Changes), out.Changes)
	}
}

func TestIdenticalTemplatesHaveNoChanges(t *testing.T) {
	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, baseSpec()),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})
	if len(out.Changes) != 0 || len(out.Findings) != 0 {
		t.Errorf("Changes=%+v Findings=%+v, want none for identical templates", out.Changes, out.Findings)
	}
	if out.Changes == nil {
		t.Error("Changes is nil; it must marshal to []")
	}
}

func TestResourceProbeAndCommandChanges(t *testing.T) {
	changed := baseSpec()
	changed.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("32Mi")
	changed.Containers[0].ReadinessProbe.InitialDelaySeconds = 1
	changed.Containers[0].Command = []string{"/app/server", "--port=8080"}

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, changed),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	memory := changeFor(t, out.Changes, "resources.limits.memory")
	if memory.From != "256Mi" || memory.To != "32Mi" {
		t.Errorf("memory change = %+v", memory)
	}
	probe := changeFor(t, out.Changes, "readinessProbe.initialDelaySeconds")
	if probe.From != "10" || probe.To != "1" {
		t.Errorf("probe change = %+v", probe)
	}
	command := changeFor(t, out.Changes, "command")
	if command.Change != "added" {
		t.Errorf("command change = %+v, want added", command)
	}
	for _, code := range []string{CodeResourcesChanged, CodeProbeChanged, CodeCommandChanged} {
		if !hasFinding(out.Findings, code) {
			t.Errorf("missing finding %q", code)
		}
	}
}

// TestSecretLiteralIsRedactedButChangeIsReported pins two properties at once:
// a literal secret value never appears in the output, and a change to it is
// still reported even though both sides redact to the same placeholder.
func TestSecretLiteralIsRedactedButChangeIsReported(t *testing.T) {
	older := baseSpec()
	older.Containers[0].Env = append(older.Containers[0].Env, corev1.EnvVar{Name: "DB_PASSWORD", Value: "old-hunter2"})
	newer := baseSpec()
	newer.Containers[0].Env = append(newer.Containers[0].Env, corev1.EnvVar{Name: "DB_PASSWORD", Value: "new-hunter3"})

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, older), replicaSetFor(2, newer),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	change := changeFor(t, out.Changes, "env.DB_PASSWORD")
	if change.Change != "modified" {
		t.Errorf("DB_PASSWORD change = %+v, want modified", change)
	}
	for _, change := range out.Changes {
		if strings.Contains(change.From+change.To, "hunter") {
			t.Errorf("secret literal leaked into output: %+v", change)
		}
	}
}

// TestSecretReferenceIsNotRedacted guards the opposite mistake: a reference
// description contains no secret material and must stay readable, even when
// its key is literally named "password".
func TestSecretReferenceIsNotRedacted(t *testing.T) {
	older := baseSpec()
	older.Containers[0].Env = append(older.Containers[0].Env, corev1.EnvVar{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password"},
	}})
	newer := baseSpec()
	newer.Containers[0].Env = append(newer.Containers[0].Env, corev1.EnvVar{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db-v2"}, Key: "password"},
	}})

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, older), replicaSetFor(2, newer),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	change := changeFor(t, out.Changes, "env.DB_PASSWORD")
	if change.From != "secretKeyRef(db/password)" || change.To != "secretKeyRef(db-v2/password)" {
		t.Errorf("reference change = %+v, want readable secretKeyRef descriptions", change)
	}
}

func TestEnvAddedAndRemoved(t *testing.T) {
	newer := baseSpec()
	newer.Containers[0].Env = []corev1.EnvVar{newer.Containers[0].Env[1], {Name: "FEATURE_X", Value: "on"}}

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, newer),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	if change := changeFor(t, out.Changes, "env.LOG_LEVEL"); change.Change != "removed" {
		t.Errorf("LOG_LEVEL = %+v, want removed", change)
	}
	if change := changeFor(t, out.Changes, "env.FEATURE_X"); change.Change != "added" || change.To != "on" {
		t.Errorf("FEATURE_X = %+v, want added with value on", change)
	}
}

// TestArgumentOrderIsNormalised: asking for 3 -> 1 still diffs old -> new,
// otherwise every "added" would read as "removed".
func TestArgumentOrderIsNormalised(t *testing.T) {
	newer := baseSpec()
	newer.Containers[0].Image = "registry.example/api:2.0.0"
	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(3, newer),
	}, RevisionDiffInput{Namespace: "default", Name: "api", FromRevision: 3, ToRevision: 1})

	if out.From.Revision != 1 || out.To.Revision != 3 {
		t.Errorf("compared %d -> %d, want normalised to 1 -> 3", out.From.Revision, out.To.Revision)
	}
	if image := changeFor(t, out.Changes, "image"); image.To != "registry.example/api:2.0.0" {
		t.Errorf("image.To = %q, want the newer image", image.To)
	}
}

// TestRevisionGapsAfterRollback: a rollback re-labels an old ReplicaSet with
// a new revision number, leaving gaps. the default must pick the previous
// retained revision, not to-1.
func TestRevisionGapsAfterRollback(t *testing.T) {
	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(2, baseSpec()), replicaSetFor(5, baseSpec()),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})
	if out.From.Revision != 2 || out.To.Revision != 5 {
		t.Errorf("compared %d -> %d, want 2 -> 5", out.From.Revision, out.To.Revision)
	}
	if len(out.Available) != 2 {
		t.Errorf("Available = %v, want [2 5]", out.Available)
	}
}

// TestForeignReplicaSetsIgnored: a ReplicaSet matching the labels but owned by
// another deployment must not be mistaken for one of this deployment's revisions.
func TestForeignReplicaSetsIgnored(t *testing.T) {
	foreign := replicaSetFor(9, baseSpec())
	foreign.Name = "other-rev9"
	foreign.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "other", UID: "other-uid"}}

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, baseSpec()), foreign,
	}, RevisionDiffInput{Namespace: "default", Name: "api"})
	if out.To.Revision != 2 {
		t.Errorf("To.Revision = %d, want 2; a foreign ReplicaSet was treated as ours", out.To.Revision)
	}
}

func TestOtherFieldsAreNamedNotInterpreted(t *testing.T) {
	newer := baseSpec()
	newer.ServiceAccountName = "api-v2"
	newer.Containers = append(newer.Containers, corev1.Container{Name: "proxy", Image: "envoy:1.30"})

	out := diffRevisions(t, []runtime.Object{
		revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, newer),
	}, RevisionDiffInput{Namespace: "default", Name: "api"})

	if change := changeFor(t, out.Changes, "pod.serviceAccountName"); change.Category != "other" {
		t.Errorf("serviceAccountName change = %+v, want category other", change)
	}
	if change := changeFor(t, out.Changes, "container"); change.Container != "proxy" || change.Change != "added" {
		t.Errorf("container change = %+v, want proxy added", change)
	}
	if !hasFinding(out.Findings, CodeOtherSpecChanged) {
		t.Error("missing other_spec_changed finding")
	}
}

func TestRevisionErrors(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		in      RevisionDiffInput
		wantIn  string
	}{
		{
			name:    "missing deployment",
			objects: nil,
			in:      RevisionDiffInput{Namespace: "default", Name: "api"},
			wantIn:  "getting deployment",
		},
		{
			name:    "single revision explains why",
			objects: []runtime.Object{revisionDeployment(), replicaSetFor(1, baseSpec())},
			in:      RevisionDiffInput{Namespace: "default", Name: "api"},
			wantIn:  "revisionHistoryLimit",
		},
		{
			name:    "unknown revision lists available ones",
			objects: []runtime.Object{revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, baseSpec())},
			in:      RevisionDiffInput{Namespace: "default", Name: "api", ToRevision: 7},
			wantIn:  "available revisions are [1 2]",
		},
		{
			name:    "same revision twice",
			objects: []runtime.Object{revisionDeployment(), replicaSetFor(1, baseSpec()), replicaSetFor(2, baseSpec())},
			in:      RevisionDiffInput{Namespace: "default", Name: "api", FromRevision: 2, ToRevision: 2},
			wantIn:  "both 2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DiffDeploymentRevisions(context.Background(), fake.NewClientset(test.objects...), test.in)
			if err == nil {
				t.Fatal("error = nil, want an error")
			}
			if !strings.Contains(err.Error(), test.wantIn) {
				t.Errorf("error %q does not contain %q", err, test.wantIn)
			}
		})
	}
}
