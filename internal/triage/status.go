package triage

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WorkloadStatusInput selects the workload to report on.
type WorkloadStatusInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the workload"`
	Name      string `json:"name" jsonschema:"name of the workload"`
	// Kind is optional. When empty, Deployment, StatefulSet and DaemonSet are
	// tried in that order, so the agent does not have to know the kind up front.
	Kind string `json:"kind,omitempty" jsonschema:"optional workload kind: Deployment, StatefulSet or DaemonSet"`
}

// ReplicaCounts normalises replica reporting across workload kinds.
//
// Deployments, StatefulSets and DaemonSets report progress through different
// status fields: DaemonSets have no spec.replicas at all and instead use
// desiredNumberScheduled/numberReady. Normalising here means the agent reasons
// about one shape rather than branching per kind.
type ReplicaCounts struct {
	Desired   int32 `json:"desired" jsonschema:"replicas the spec asks for"`
	Ready     int32 `json:"ready" jsonschema:"replicas passing their readiness probe"`
	Available int32 `json:"available" jsonschema:"replicas ready for at least minReadySeconds"`
	Updated   int32 `json:"updated" jsonschema:"replicas running the current template"`
}

// WorkloadCondition is a trimmed status condition. The full Kubernetes
// condition carries fields the model does not need; this keeps the useful ones.
type WorkloadCondition struct {
	Type               string `json:"type" jsonschema:"condition type, e.g. Available or Progressing"`
	Status             string `json:"status" jsonschema:"True, False or Unknown"`
	Reason             string `json:"reason,omitempty" jsonschema:"machine-readable reason for the current status"`
	Message            string `json:"message,omitempty" jsonschema:"human-readable detail from the controller"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty" jsonschema:"RFC3339 time the condition last changed"`
}

// WorkloadStatusOutput is the structured answer to "is this workload healthy".
type WorkloadStatusOutput struct {
	Object     ObjectRef           `json:"object" jsonschema:"the workload this status describes"`
	Replicas   ReplicaCounts       `json:"replicas" jsonschema:"normalised replica counts"`
	Conditions []WorkloadCondition `json:"conditions,omitempty" jsonschema:"status conditions reported by the controller"`
	AgeSeconds int64               `json:"ageSeconds" jsonschema:"seconds since the workload was created"`
	Generation GenerationStatus    `json:"generation" jsonschema:"whether the controller has observed the latest spec"`
	Healthy    bool                `json:"healthy" jsonschema:"true when all desired replicas are ready and no warning findings were raised"`
	Findings   []Finding           `json:"findings" jsonschema:"structured observations about this workload"`
}

// GenerationStatus exposes the spec-versus-observed gap.
// if observedGeneration lags
// metadata.generation, the controller has not yet acted on the newest spec, so
// every other status field describes the *previous* spec. An agent that misses
// this will confidently diagnose stale data.
type GenerationStatus struct {
	Generation         int64 `json:"generation" jsonschema:"metadata.generation of the current spec"`
	ObservedGeneration int64 `json:"observedGeneration" jsonschema:"generation the controller has processed"`
	UpToDate           bool  `json:"upToDate" jsonschema:"false when the controller has not yet observed the current spec"`
}

// GetWorkloadStatus reports replica health and rollout state for a workload.
//
// It returns an error when the workload does not exist: the agent asked about
// something absent and should know that, rather than receiving an empty report
// it might read as "healthy". A workload that exists and is fine returns an
// empty Findings slice.
func GetWorkloadStatus(ctx context.Context, clientset kubernetes.Interface, in WorkloadStatusInput) (WorkloadStatusOutput, error) {
	kinds := []string{in.Kind}
	if in.Kind == "" {
		kinds = []string{"Deployment", "StatefulSet", "DaemonSet"}
	}

	var lastErr error
	for _, kind := range kinds {
		out, err := statusForKind(ctx, clientset, kind, in.Namespace, in.Name)
		if err == nil {
			return out, nil
		}
		lastErr = err
		// only fall through to the next kind when this one simply was not found.
		if !apierrors.IsNotFound(err) {
			return WorkloadStatusOutput{}, err
		}
	}

	if in.Kind == "" {
		return WorkloadStatusOutput{}, fmt.Errorf(
			"no Deployment, StatefulSet or DaemonSet named %q in namespace %q", in.Name, in.Namespace)
	}
	return WorkloadStatusOutput{}, lastErr
}

// statusForKind fetches one specific workload kind and normalises it.
func statusForKind(ctx context.Context, clientset kubernetes.Interface, kind, namespace, name string) (WorkloadStatusOutput, error) {
	switch kind {
	case "Deployment":
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatusOutput{}, err
		}
		return fromDeployment(deployment), nil

	case "StatefulSet":
		statefulSet, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatusOutput{}, err
		}
		return fromStatefulSet(statefulSet), nil

	case "DaemonSet":
		daemonSet, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatusOutput{}, err
		}
		return fromDaemonSet(daemonSet), nil

	default:
		return WorkloadStatusOutput{}, fmt.Errorf(
			"unsupported kind %q: want Deployment, StatefulSet or DaemonSet", kind)
	}
}

func fromDeployment(deployment *appsv1.Deployment) WorkloadStatusOutput {
	desired := int32(1) // spec.replicas is optional and defaults to 1
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	conditions := make([]WorkloadCondition, 0, len(deployment.Status.Conditions))
	for _, condition := range deployment.Status.Conditions {
		conditions = append(conditions, WorkloadCondition{
			Type:               string(condition.Type),
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: formatTime(condition.LastTransitionTime),
		})
	}

	out := WorkloadStatusOutput{
		Object: ObjectRef{Kind: "Deployment", Namespace: deployment.Namespace, Name: deployment.Name},
		Replicas: ReplicaCounts{
			Desired:   desired,
			Ready:     deployment.Status.ReadyReplicas,
			Available: deployment.Status.AvailableReplicas,
			Updated:   deployment.Status.UpdatedReplicas,
		},
		Conditions: conditions,
		AgeSeconds: ageSeconds(deployment.CreationTimestamp),
		Generation: generationStatus(deployment.Generation, deployment.Status.ObservedGeneration),
	}
	out.Findings = analyseWorkload(out, conditions)
	out.Healthy = isHealthy(out)
	return out
}

func fromStatefulSet(statefulSet *appsv1.StatefulSet) WorkloadStatusOutput {
	desired := int32(1)
	if statefulSet.Spec.Replicas != nil {
		desired = *statefulSet.Spec.Replicas
	}

	conditions := make([]WorkloadCondition, 0, len(statefulSet.Status.Conditions))
	for _, condition := range statefulSet.Status.Conditions {
		conditions = append(conditions, WorkloadCondition{
			Type:               string(condition.Type),
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: formatTime(condition.LastTransitionTime),
		})
	}

	out := WorkloadStatusOutput{
		Object: ObjectRef{Kind: "StatefulSet", Namespace: statefulSet.Namespace, Name: statefulSet.Name},
		Replicas: ReplicaCounts{
			Desired:   desired,
			Ready:     statefulSet.Status.ReadyReplicas,
			Available: statefulSet.Status.AvailableReplicas,
			Updated:   statefulSet.Status.UpdatedReplicas,
		},
		Conditions: conditions,
		AgeSeconds: ageSeconds(statefulSet.CreationTimestamp),
		Generation: generationStatus(statefulSet.Generation, statefulSet.Status.ObservedGeneration),
	}
	out.Findings = analyseWorkload(out, conditions)
	out.Healthy = isHealthy(out)
	return out
}

func fromDaemonSet(daemonSet *appsv1.DaemonSet) WorkloadStatusOutput {
	// DaemonSets have no spec.replicas: the desired count is derived from how
	// many nodes match the node selector, so it lives in status.
	status := daemonSet.Status

	conditions := make([]WorkloadCondition, 0, len(status.Conditions))
	for _, condition := range status.Conditions {
		conditions = append(conditions, WorkloadCondition{
			Type:               string(condition.Type),
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: formatTime(condition.LastTransitionTime),
		})
	}

	out := WorkloadStatusOutput{
		Object: ObjectRef{Kind: "DaemonSet", Namespace: daemonSet.Namespace, Name: daemonSet.Name},
		Replicas: ReplicaCounts{
			Desired:   status.DesiredNumberScheduled,
			Ready:     status.NumberReady,
			Available: status.NumberAvailable,
			Updated:   status.UpdatedNumberScheduled,
		},
		Conditions: conditions,
		AgeSeconds: ageSeconds(daemonSet.CreationTimestamp),
		Generation: generationStatus(daemonSet.Generation, status.ObservedGeneration),
	}
	out.Findings = analyseWorkload(out, conditions)
	out.Healthy = isHealthy(out)
	return out
}

// analyseWorkload turns normalised status into findings.
func analyseWorkload(out WorkloadStatusOutput, conditions []WorkloadCondition) []Finding {
	findings := []Finding{}

	if !out.Generation.UpToDate {
		findings = append(findings, NewFinding(
			CodeRolloutStuck, SeverityInfo,
			"controller has not yet observed the latest spec; status may describe the previous revision").
			WithEvidence("generation", out.Generation.Generation).
			WithEvidence("observedGeneration", out.Generation.ObservedGeneration))
	}

	if out.Replicas.Ready < out.Replicas.Desired {
		findings = append(findings, NewFinding(
			CodeReplicasUnavailable, SeverityCritical,
			"fewer replicas are ready than desired").
			WithEvidence("desired", out.Replicas.Desired).
			WithEvidence("ready", out.Replicas.Ready).
			WithEvidence("missing", out.Replicas.Desired-out.Replicas.Ready))
	}

	for _, condition := range conditions {
		// ProgressDeadlineExceeded surfaces as Progressing=False with this
		// reason. It is the controller giving up on the rollout, which is
		// strictly more informative than the replica gap alone.
		if condition.Type == "Progressing" && condition.Status == "False" {
			if condition.Reason == "ProgressDeadlineExceeded" {
				findings = append(findings, NewFinding(
					CodeProgressDeadline, SeverityCritical,
					"rollout exceeded its progress deadline and the controller stopped waiting").
					WithEvidence("reason", condition.Reason).
					WithEvidence("message", condition.Message))
				continue
			}
			findings = append(findings, NewFinding(
				CodeRolloutStuck, SeverityWarning,
				"rollout is not progressing").
				WithEvidence("reason", condition.Reason).
				WithEvidence("message", condition.Message))
		}
	}

	return findings
}

// isHealthy is intentionally strict: all desired replicas ready, the controller
// up to date with the spec, and nothing worse than an info finding.
func isHealthy(out WorkloadStatusOutput) bool {
	// a stale observedGeneration means every status field below describes the
	// *previous* spec. reporting healthy from stale data would be asserting
	// something we cannot actually know, so withhold the claim.
	if !out.Generation.UpToDate {
		return false
	}
	if out.Replicas.Desired == 0 {
		// a deliberately scaled-to-zero workload is not unhealthy.
		return true
	}
	if out.Replicas.Ready < out.Replicas.Desired {
		return false
	}
	for _, finding := range out.Findings {
		if finding.Severity == SeverityCritical || finding.Severity == SeverityWarning {
			return false
		}
	}
	return true
}

func generationStatus(generation, observed int64) GenerationStatus {
	return GenerationStatus{
		Generation:         generation,
		ObservedGeneration: observed,
		UpToDate:           observed >= generation,
	}
}

func ageSeconds(creation metav1.Time) int64 {
	if creation.IsZero() {
		return 0
	}
	return int64(time.Since(creation.Time).Seconds())
}

func formatTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
