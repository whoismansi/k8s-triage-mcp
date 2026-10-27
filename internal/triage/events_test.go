package triage

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

var eventSequence int

// coreEvent builds a core v1 event about a pod, last seen `ago` before now.
func coreEvent(podName, eventType, reason, message string, count int32, ago time.Duration) *corev1.Event {
	eventSequence++
	seen := metav1.NewTime(time.Now().Add(-ago))
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("event-%d", eventSequence), Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: podName, Namespace: "default"},
		Type:           eventType,
		Reason:         reason,
		Message:        message,
		Count:          count,
		FirstTimestamp: seen,
		LastTimestamp:  seen,
		Source:         corev1.EventSource{Component: "kubelet"},
	}
}

func podEvents(t *testing.T, objects []runtime.Object, in PodEventsInput) PodEventsOutput {
	t.Helper()
	out, err := GetPodEvents(context.Background(), fake.NewClientset(objects...), in)
	if err != nil {
		t.Fatalf("GetPodEvents() error = %v", err)
	}
	return out
}

// TestNoiseFiltering is the core value of this tool: routine lifecycle events
// are hidden, warnings and diagnostic Normal events are kept, and the output
// says how much was hidden.
func TestNoiseFiltering(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("web-1", "Normal", "Scheduled", "Successfully assigned default/web-1", 1, time.Minute),
		coreEvent("web-1", "Normal", "Pulling", "Pulling image app:v2", 1, time.Minute),
		coreEvent("web-1", "Normal", "Pulled", "Successfully pulled image", 1, time.Minute),
		coreEvent("web-1", "Normal", "Created", "Created container app", 1, time.Minute),
		coreEvent("web-1", "Normal", "Killing", "Container app failed liveness probe, will be restarted", 1, time.Minute),
		coreEvent("web-1", "Warning", "BackOff", "Back-off restarting failed container", 12, time.Minute),
	}

	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "web-1"})

	if len(out.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2 (Killing and BackOff); got %+v", len(out.Events), out.Events)
	}
	if out.Filtered != 4 {
		t.Errorf("Filtered = %d, want 4 routine events hidden", out.Filtered)
	}
	if out.TotalMatched != 6 {
		t.Errorf("TotalMatched = %d, want 6", out.TotalMatched)
	}

	all := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "web-1", IncludeAll: true})
	if len(all.Events) != 6 || all.Filtered != 0 {
		t.Errorf("IncludeAll: len(Events)=%d Filtered=%d, want 6 and 0", len(all.Events), all.Filtered)
	}
}

func TestEventsAreNewestFirst(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("p", "Warning", "Unhealthy", "Readiness probe failed", 1, 30*time.Minute),
		coreEvent("p", "Warning", "BackOff", "Back-off restarting failed container", 1, time.Minute),
		coreEvent("p", "Warning", "Failed", "Error: ImagePullBackOff", 1, 10*time.Minute),
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})

	want := []string{"BackOff", "Failed", "Unhealthy"}
	for index, reason := range want {
		if out.Events[index].Reason != reason {
			t.Errorf("Events[%d].Reason = %q, want %q (newest first)", index, out.Events[index].Reason, reason)
		}
	}
}

func TestWindowExcludesOldEvents(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("p", "Warning", "BackOff", "recent", 1, 5*time.Minute),
		coreEvent("p", "Warning", "BackOff", "ancient", 1, 3*time.Hour),
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})
	if len(out.Events) != 1 || out.Events[0].Message != "recent" {
		t.Errorf("Events = %+v, want only the recent one in the default 60 minute window", out.Events)
	}
	if out.WindowMinutes != 60 {
		t.Errorf("WindowMinutes = %d, want 60", out.WindowMinutes)
	}

	wide := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p", SinceMinutes: 240})
	if len(wide.Events) != 2 {
		t.Errorf("SinceMinutes=240: len(Events) = %d, want 2", len(wide.Events))
	}
}

// TestDuplicateAcrossAPIsIsMerged covers the same occurrence surfacing in both
// core v1 and events.k8s.io: it must appear once with its count not doubled.
func TestDuplicateAcrossAPIsIsMerged(t *testing.T) {
	seen := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	objects := []runtime.Object{
		coreEvent("p", "Warning", "BackOff", "Back-off restarting failed container", 7, time.Minute),
		&eventsv1.Event{
			ObjectMeta:          metav1.ObjectMeta{Name: "v1-event", Namespace: "default"},
			Regarding:           corev1.ObjectReference{Kind: "Pod", Name: "p", Namespace: "default"},
			Type:                "Warning",
			Reason:              "BackOff",
			Note:                "Back-off restarting failed container",
			EventTime:           seen,
			ReportingController: "kubelet",
			DeprecatedCount:     7,
		},
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})
	if len(out.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1 after merging both APIs; got %+v", len(out.Events), out.Events)
	}
	if out.Events[0].Count != 7 {
		t.Errorf("Count = %d, want 7 (not summed to 14)", out.Events[0].Count)
	}
}

// TestEventsV1Only covers clusters where only events.k8s.io is populated.
func TestEventsV1Only(t *testing.T) {
	objects := []runtime.Object{&eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "only-v1", Namespace: "default"},
		Regarding:  corev1.ObjectReference{Kind: "Pod", Name: "p", Namespace: "default"},
		Type:       "Warning",
		Reason:     "FailedScheduling",
		Note:       "0/3 nodes are available: 3 Insufficient cpu.",
		EventTime:  metav1.NewMicroTime(time.Now().Add(-time.Minute)),
		Series:     &eventsv1.EventSeries{Count: 9, LastObservedTime: metav1.NewMicroTime(time.Now())},
	}}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})
	if len(out.Events) != 1 || out.Events[0].Count != 9 {
		t.Fatalf("Events = %+v, want one event with series count 9", out.Events)
	}
	if !hasFinding(out.Findings, CodeUnschedulable) {
		t.Errorf("missing unschedulable finding; got %+v", out.Findings)
	}
}

func TestEventsForOtherPodsAreExcluded(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("target", "Warning", "BackOff", "mine", 1, time.Minute),
		coreEvent("neighbour", "Warning", "BackOff", "not mine", 1, time.Minute),
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "target"})
	if len(out.Events) != 1 || out.Events[0].Message != "mine" {
		t.Errorf("Events = %+v, want only the target pod's event", out.Events)
	}
}

func TestLabelSelectorResolvesPods(t *testing.T) {
	objects := []runtime.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{"app": "web"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "default", Labels: map[string]string{"app": "db"}}},
		coreEvent("web-1", "Warning", "BackOff", "web-1 crashing", 1, time.Minute),
		coreEvent("web-2", "Warning", "Unhealthy", "web-2 probe", 1, time.Minute),
		coreEvent("db-1", "Warning", "BackOff", "db crashing", 1, time.Minute),
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", LabelSelector: "app=web"})

	if len(out.Pods) != 2 {
		t.Errorf("Pods = %v, want web-1 and web-2", out.Pods)
	}
	if len(out.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2 from the web pods only", len(out.Events))
	}
	for _, event := range out.Events {
		if event.Object.Name == "db-1" {
			t.Error("event from db-1 leaked into an app=web query")
		}
	}
}

func TestLimitTruncates(t *testing.T) {
	var objects []runtime.Object
	for index := range 10 {
		objects = append(objects, coreEvent("p", "Warning", "BackOff", fmt.Sprintf("message %d", index), 1, time.Duration(index+1)*time.Minute))
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p", Limit: 3})
	if len(out.Events) != 3 || !out.Truncated {
		t.Errorf("len(Events)=%d Truncated=%v, want 3 and true", len(out.Events), out.Truncated)
	}
	if out.Events[0].Message != "message 0" {
		t.Errorf("Events[0] = %q, want the newest event kept after truncation", out.Events[0].Message)
	}
}

func TestWarningClassification(t *testing.T) {
	tests := []struct {
		reason, message, wantCode string
	}{
		{"BackOff", "Back-off restarting failed container app", CodeCrashLoopBackOff},
		{"BackOff", `Back-off pulling image "app:tpyo"`, CodeImagePullFailed},
		{"Failed", "Error: ErrImagePull", CodeImagePullFailed},
		{"Failed", "Failed to pull image \"app:tpyo\": not found", CodeImagePullFailed},
		{"Unhealthy", "Readiness probe failed: connection refused", CodeProbeFailing},
		{"FailedScheduling", "0/1 nodes are available", CodeUnschedulable},
		{"FailedMount", "MountVolume.SetUp failed: secret \"db\" not found", CodeVolumeMountFailed},
		{"Evicted", "The node was low on resource: memory.", CodeEvicted},
	}
	for _, test := range tests {
		t.Run(test.reason+"/"+test.wantCode, func(t *testing.T) {
			out := podEvents(t, []runtime.Object{coreEvent("p", "Warning", test.reason, test.message, 1, time.Minute)},
				PodEventsInput{Namespace: "default", Pod: "p"})
			if !hasFinding(out.Findings, test.wantCode) {
				t.Errorf("missing %q for %s %q; got %+v", test.wantCode, test.reason, test.message, out.Findings)
			}
		})
	}
}

// TestFindingsDedupedPerPod: forty BackOff events are one problem, not forty.
func TestFindingsDedupedPerPod(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("p", "Warning", "BackOff", "Back-off restarting failed container app", 40, time.Minute),
		coreEvent("p", "Warning", "BackOff", "Back-off restarting failed container sidecar", 3, time.Minute),
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})
	count := 0
	for _, finding := range out.Findings {
		if finding.Code == CodeCrashLoopBackOff {
			count++
		}
	}
	if count != 1 {
		t.Errorf("crashloop_backoff findings = %d, want 1 per pod", count)
	}
}

func TestEmptyResultIsNotAnError(t *testing.T) {
	out := podEvents(t, nil, PodEventsInput{Namespace: "default", Pod: "quiet"})
	if out.Events == nil || out.Findings == nil {
		t.Error("Events and Findings must be empty slices, not nil, so they marshal to []")
	}
}

func TestInputValidation(t *testing.T) {
	clientset := fake.NewClientset()
	for _, in := range []PodEventsInput{
		{Namespace: "default"},
		{Namespace: "default", Pod: "p", LabelSelector: "app=web"},
	} {
		if _, err := GetPodEvents(context.Background(), clientset, in); err == nil {
			t.Errorf("GetPodEvents(%+v) error = nil, want an error", in)
		}
	}
	if _, err := GetPodEvents(context.Background(), clientset,
		PodEventsInput{Namespace: "default", LabelSelector: "app=missing"}); err == nil {
		t.Error("selector matching no pods returned no error")
	}
}
