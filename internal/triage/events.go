package triage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// event window and size limits. events are cheap individually but a noisy
// namespace produces hundreds, and each one costs the agent context.
const (
	DefaultEventWindow = time.Hour
	DefaultEventLimit  = 30
	MaxEventLimit      = 100
	// maxPodsPerSelector stops a broad selector (app=web on a large fleet)
	// from fanning out into one events query per pod.
	maxPodsPerSelector = 50
)

// diagnosticNormalReasons are Normal events worth keeping by default. Most
// Normal events are routine lifecycle chatter (Scheduled, Pulling, Pulled,
// Created, Started) that bury the warning that matters; these few carry real
// diagnostic signal.
var diagnosticNormalReasons = map[string]bool{
	"Killing":           true, // kubelet stopping a container, e.g. after a failed liveness probe
	"Preempted":         true, // evicted to make room for a higher-priority pod
	"SuccessfulCreate":  true, // a controller created a pod, useful for rollout timelines
	"SuccessfulDelete":  true,
	"ScalingReplicaSet": true, // deployment rollout steps
	"NodeNotReady":      true,
}

// PodEventsInput selects which pods' events to return.
type PodEventsInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace to search"`
	// exactly one of Pod or LabelSelector should be set.
	Pod           string `json:"pod,omitempty" jsonschema:"pod name; set this or labelSelector"`
	LabelSelector string `json:"labelSelector,omitempty" jsonschema:"label selector such as app=web; set this or pod"`
	SinceMinutes  int    `json:"sinceMinutes,omitempty" jsonschema:"only events seen within this many minutes; default 60"`
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum events to return, newest first; default 30, capped at 100"`
	// IncludeAll disables noise filtering. Off by default because routine
	// Normal events usually outnumber the useful ones many times over.
	IncludeAll bool `json:"includeAll,omitempty" jsonschema:"include routine Normal events that are filtered out by default"`
}

// PodEvent is one normalised, deduplicated event.
type PodEvent struct {
	Type    string `json:"type" jsonschema:"Normal or Warning"`
	Reason  string `json:"reason" jsonschema:"machine-readable reason, e.g. BackOff, FailedScheduling"`
	Message string `json:"message" jsonschema:"event message"`
	// Object is the pod or other object the event is about.
	Object ObjectRef `json:"object" jsonschema:"object the event is about"`
	// Count merges repeats: fifty identical BackOff events become one entry.
	Count     int32  `json:"count" jsonschema:"how many times this event occurred"`
	FirstSeen string `json:"firstSeen,omitempty" jsonschema:"RFC3339 time first seen"`
	LastSeen  string `json:"lastSeen" jsonschema:"RFC3339 time last seen"`
	Source    string `json:"source,omitempty" jsonschema:"component that reported it, e.g. kubelet or default-scheduler"`
}

// PodEventsOutput is the filtered event timeline.
type PodEventsOutput struct {
	Namespace string `json:"namespace" jsonschema:"namespace searched"`
	// Pods lists the pods whose events were searched, so the agent knows the
	// scope of what it did and did not see.
	Pods   []string   `json:"pods" jsonschema:"pods whose events were searched"`
	Events []PodEvent `json:"events" jsonschema:"events, newest first"`
	// Window states the time range, so an empty result reads as "nothing in
	// the last hour" rather than "nothing ever".
	WindowMinutes int `json:"windowMinutes" jsonschema:"time window searched, in minutes"`
	// TotalMatched and Filtered let the agent tell "quiet" from "filtered".
	TotalMatched int  `json:"totalMatched" jsonschema:"events in the window before noise filtering and the limit"`
	Filtered     int  `json:"filtered" jsonschema:"routine Normal events hidden by noise filtering"`
	Truncated    bool `json:"truncated" jsonschema:"true when more events matched than the limit allowed"`
	// PodsTruncated is set when the selector matched more pods than searched.
	PodsTruncated bool      `json:"podsTruncated,omitempty" jsonschema:"true when the selector matched more pods than were searched"`
	Findings      []Finding `json:"findings" jsonschema:"structured observations drawn from warning events"`
}

// GetPodEvents returns recent, noise-filtered events for a pod or selector.
func GetPodEvents(ctx context.Context, clientset kubernetes.Interface, in PodEventsInput) (PodEventsOutput, error) {
	if (in.Pod == "") == (in.LabelSelector == "") {
		return PodEventsOutput{}, fmt.Errorf("set exactly one of pod or labelSelector")
	}

	window := DefaultEventWindow
	if in.SinceMinutes > 0 {
		window = time.Duration(in.SinceMinutes) * time.Minute
	}
	limit := DefaultEventLimit
	if in.Limit > 0 {
		limit = min(in.Limit, MaxEventLimit)
	}

	pods, podsTruncated, err := resolvePods(ctx, clientset, in)
	if err != nil {
		return PodEventsOutput{}, err
	}

	// both APIs are read because events moved from core v1 to events.k8s.io,
	// and which one a cluster populates depends on its version and components.
	// the merge below dedupes events that appear in both.
	collected := map[string]*PodEvent{}
	for _, podName := range pods {
		if err := collectCoreEvents(ctx, clientset, in.Namespace, podName, collected); err != nil {
			return PodEventsOutput{}, err
		}
		// events.k8s.io is best effort: a cluster may not serve it, and the
		// core API alone is still a complete answer on those clusters.
		_ = collectEventsV1(ctx, clientset, in.Namespace, podName, collected)
	}

	cutoff := time.Now().Add(-window)
	var inWindow, kept []PodEvent
	for _, event := range collected {
		lastSeen, parseErr := time.Parse(time.RFC3339, event.LastSeen)
		if parseErr == nil && lastSeen.Before(cutoff) {
			continue
		}
		inWindow = append(inWindow, *event)
	}
	filtered := 0
	for _, event := range inWindow {
		if !in.IncludeAll && event.Type == corev1.EventTypeNormal && !diagnosticNormalReasons[event.Reason] {
			filtered++
			continue
		}
		kept = append(kept, event)
	}

	// newest first: the most recent failure is usually the one that matters.
	// ties break on reason then message so output is deterministic.
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].LastSeen != kept[j].LastSeen {
			return kept[i].LastSeen > kept[j].LastSeen
		}
		if kept[i].Reason != kept[j].Reason {
			return kept[i].Reason < kept[j].Reason
		}
		return kept[i].Message < kept[j].Message
	})

	out := PodEventsOutput{
		Namespace:     in.Namespace,
		Pods:          pods,
		Events:        []PodEvent{},
		WindowMinutes: int(window / time.Minute),
		TotalMatched:  len(inWindow),
		Filtered:      filtered,
		PodsTruncated: podsTruncated,
	}
	if len(kept) > limit {
		out.Truncated = true
		kept = kept[:limit]
	}
	if kept != nil {
		out.Events = kept
	}
	out.Findings = analyseEvents(out.Events)
	return out, nil
}

// resolvePods turns the input into a concrete pod list.
func resolvePods(ctx context.Context, clientset kubernetes.Interface, in PodEventsInput) ([]string, bool, error) {
	if in.Pod != "" {
		// the pod may already be gone (evicted, replaced in a rollout) while
		// its events remain, so a missing pod is not an error here.
		return []string{in.Pod}, false, nil
	}

	list, err := clientset.CoreV1().Pods(in.Namespace).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
	if err != nil {
		return nil, false, fmt.Errorf("listing pods for selector %q: %w", in.LabelSelector, err)
	}
	if len(list.Items) == 0 {
		return nil, false, fmt.Errorf("no pods in namespace %q match selector %q", in.Namespace, in.LabelSelector)
	}

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	if len(names) > maxPodsPerSelector {
		return names[:maxPodsPerSelector], true, nil
	}
	return names, false, nil
}

func collectCoreEvents(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, collected map[string]*PodEvent) error {
	selector := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": podName,
	}.AsSelector().String()

	list, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return fmt.Errorf("listing events for pod %q: %w", podName, err)
	}
	for _, event := range list.Items {
		// the fake clientset ignores field selectors, so filter again here.
		// this is also harmless against a real API server.
		if event.InvolvedObject.Kind != "Pod" || event.InvolvedObject.Name != podName {
			continue
		}
		count := event.Count
		if event.Series != nil && event.Series.Count > count {
			count = event.Series.Count
		}
		lastSeen := latestTime(event.LastTimestamp.Time, event.EventTime.Time, event.FirstTimestamp.Time)
		if event.Series != nil {
			lastSeen = latestTime(lastSeen, event.Series.LastObservedTime.Time)
		}
		merge(collected, PodEvent{
			Type:      event.Type,
			Reason:    event.Reason,
			Message:   event.Message,
			Object:    ObjectRef{Kind: event.InvolvedObject.Kind, Namespace: namespace, Name: event.InvolvedObject.Name},
			Count:     max(count, 1),
			FirstSeen: formatGoTime(earliestTime(event.FirstTimestamp.Time, event.EventTime.Time)),
			LastSeen:  formatGoTime(lastSeen),
			Source:    firstNonEmpty(event.Source.Component, event.ReportingController),
		})
	}
	return nil
}

func collectEventsV1(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, collected map[string]*PodEvent) error {
	selector := fields.Set{
		"regarding.kind": "Pod",
		"regarding.name": podName,
	}.AsSelector().String()

	list, err := clientset.EventsV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return err
	}
	for _, event := range list.Items {
		if event.Regarding.Kind != "Pod" || event.Regarding.Name != podName {
			continue
		}
		merge(collected, fromEventsV1(namespace, event))
	}
	return nil
}

func fromEventsV1(namespace string, event eventsv1.Event) PodEvent {
	count := event.DeprecatedCount
	lastSeen := latestTime(event.EventTime.Time, event.DeprecatedLastTimestamp.Time, event.DeprecatedFirstTimestamp.Time)
	if event.Series != nil {
		count = max(count, event.Series.Count)
		lastSeen = latestTime(lastSeen, event.Series.LastObservedTime.Time)
	}
	return PodEvent{
		Type:      event.Type,
		Reason:    event.Reason,
		Message:   event.Note,
		Object:    ObjectRef{Kind: event.Regarding.Kind, Namespace: namespace, Name: event.Regarding.Name},
		Count:     max(count, 1),
		FirstSeen: formatGoTime(earliestTime(event.DeprecatedFirstTimestamp.Time, event.EventTime.Time)),
		LastSeen:  formatGoTime(lastSeen),
		Source:    firstNonEmpty(event.ReportingController, event.DeprecatedSource.Component),
	}
}

// merge folds an event into the collection. events are keyed on what they say
// rather than their object name, because the same occurrence can surface from
// both APIs and the same message repeats under different event objects.
func merge(collected map[string]*PodEvent, event PodEvent) {
	key := strings.Join([]string{event.Object.Name, event.Type, event.Reason, event.Message}, "\x00")
	existing, found := collected[key]
	if !found {
		copied := event
		collected[key] = &copied
		return
	}
	// the same occurrence seen through both APIs reports the same count, so
	// keep the larger rather than summing, which would double it.
	existing.Count = max(existing.Count, event.Count)
	if event.LastSeen > existing.LastSeen {
		existing.LastSeen = event.LastSeen
	}
	if event.FirstSeen != "" && (existing.FirstSeen == "" || event.FirstSeen < existing.FirstSeen) {
		existing.FirstSeen = event.FirstSeen
	}
	if existing.Source == "" {
		existing.Source = event.Source
	}
}

// analyseEvents maps warning reasons to findings. one finding per reason and
// object: the event list already carries every message, so repeating them as
// findings would only add noise.
func analyseEvents(events []PodEvent) []Finding {
	findings := []Finding{}
	seen := map[string]bool{}

	for _, event := range events {
		if event.Type != corev1.EventTypeWarning {
			continue
		}
		code, severity, message := classifyWarning(event)
		if code == "" {
			continue
		}
		key := code + "\x00" + event.Object.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, NewFinding(code, severity, message).
			WithEvidence("pod", event.Object.Name).
			WithEvidence("reason", event.Reason).
			WithEvidence("message", event.Message).
			WithEvidence("count", event.Count).
			WithEvidence("lastSeen", event.LastSeen))
	}
	return findings
}

func classifyWarning(event PodEvent) (string, Severity, string) {
	switch event.Reason {
	case "BackOff":
		// "Back-off pulling image" and "Back-off restarting failed container"
		// share a reason, so the message decides which it is.
		if strings.Contains(event.Message, "pulling image") {
			return CodeImagePullFailed, SeverityCritical, "kubelet is backing off pulling the image"
		}
		return CodeCrashLoopBackOff, SeverityCritical, "kubelet is backing off restarting a failed container"
	case "Failed":
		// kubelet messages vary in case ("Failed to pull image", "Error:
		// ErrImagePull"), so match case-insensitively.
		lowered := strings.ToLower(event.Message)
		if strings.Contains(lowered, "pull") || strings.Contains(lowered, "image") {
			return CodeImagePullFailed, SeverityCritical, "image pull failed"
		}
		return CodeContainerStartFailed, SeverityCritical, "container failed to start"
	case "InspectFailed", "ErrImageNeverPull":
		return CodeImagePullFailed, SeverityCritical, "image could not be used"
	case "Unhealthy":
		return CodeProbeFailing, SeverityWarning, "a probe is failing"
	case "FailedScheduling":
		return CodeUnschedulable, SeverityCritical, "scheduler cannot place the pod"
	case "FailedMount", "FailedAttachVolume":
		return CodeVolumeMountFailed, SeverityCritical, "a volume could not be attached or mounted"
	case "Evicted":
		return CodeEvicted, SeverityCritical, "pod was evicted from its node"
	case "OOMKilling":
		return CodeOOMKilled, SeverityCritical, "kernel OOM killer killed a process"
	case "FailedCreatePodSandBox", "FailedCreatePodContainer":
		return CodePodCreateFailed, SeverityCritical, "pod sandbox or container could not be created"
	}
	return "", "", ""
}

func latestTime(times ...time.Time) time.Time {
	var latest time.Time
	for _, candidate := range times {
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

func earliestTime(times ...time.Time) time.Time {
	var earliest time.Time
	for _, candidate := range times {
		if candidate.IsZero() {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest
}

func formatGoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
