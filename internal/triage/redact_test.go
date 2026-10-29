package triage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// fake credentials are assembled at runtime so the source never contains a
// complete token literal. secret scanners match on shape and would otherwise
// flag these fixtures, or block a push, even though they are not real.
var (
	fakeAWSKey      = "AKIA" + strings.Repeat("X", 16)
	fakeGitHubToken = "ghp" + "_" + strings.Repeat("a", 36)
	fakePEMHeader   = "-----BEGIN RSA " + "PRIVATE KEY-----"
	fakeJWT         = "eyJ" + strings.Repeat("h", 12) + "." + strings.Repeat("p", 12) + "." + strings.Repeat("s", 12)
)

func TestRedactLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// mustHide are substrings that must not survive redaction.
		mustHide []string
		// mustKeep are substrings that must survive, so the line stays useful.
		mustKeep []string
	}{
		{
			name:     "password in a database URL keeps host and user",
			in:       "connecting to postgres://app:s3cr3tPass@db.internal:5432/orders",
			mustHide: []string{"s3cr3tPass"},
			mustKeep: []string{"postgres://app:", "@db.internal:5432/orders"},
		},
		{
			name:     "bearer token",
			in:       "request failed: Authorization: Bearer abcDEF123456789ghiJKL",
			mustHide: []string{"abcDEF123456789ghiJKL"},
			mustKeep: []string{"Bearer"},
		},
		{
			name:     "jwt",
			in:       "token=" + fakeJWT,
			mustHide: []string{fakeJWT},
		},
		{
			name:     "env style secret keeps the key name",
			in:       "loaded config DB_PASSWORD=hunter2 PORT=8080",
			mustHide: []string{"hunter2"},
			mustKeep: []string{"DB_PASSWORD=", "PORT=8080"},
		},
		{
			name:     "quoted json secret",
			in:       `{"api_key": "abc123secretvalue", "region": "us-east-1"}`,
			mustHide: []string{"abc123secretvalue"},
			mustKeep: []string{`"region": "us-east-1"`},
		},
		{
			name:     "aws access key id",
			in:       "using credentials " + fakeAWSKey,
			mustHide: []string{fakeAWSKey},
		},
		{
			name:     "github token",
			in:       "clone with " + fakeGitHubToken,
			mustHide: []string{fakeGitHubToken},
		},
		{
			name:     "private key header",
			in:       fakePEMHeader,
			mustHide: []string{"BEGIN RSA PRIVATE KEY"},
		},
		{
			name:     "token authorization scheme",
			in:       "request failed: Authorization: Token abc123def456ghi789",
			mustHide: []string{"abc123def456ghi789"},
			mustKeep: []string{"Token"},
		},
		{
			name:     "url with password and no user",
			in:       "connecting to redis://:onlyPasswordNoUser@cache:6379",
			mustHide: []string{"onlyPasswordNoUser"},
			mustKeep: []string{"redis://:", "@cache:6379"},
		},
		{
			name:     "password flag with a space",
			in:       "exec: /app/migrate --password hunter2 --host db",
			mustHide: []string{"hunter2"},
			mustKeep: []string{"--password", "--host db"},
		},
		{
			name:     "short pass key name",
			in:       "env DB_PASS=hunter2 PORT=8080",
			mustHide: []string{"hunter2"},
			mustKeep: []string{"DB_PASS=", "PORT=8080"},
		},
		{
			name:     "pwd key name",
			in:       "connection string pwd=hunter2;server=db",
			mustHide: []string{"hunter2"},
			mustKeep: []string{"pwd=", "server=db"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, count := redactLine(test.in)
			if count == 0 {
				t.Errorf("redactLine(%q) redacted nothing", test.in)
			}
			for _, secret := range test.mustHide {
				if strings.Contains(got, secret) {
					t.Errorf("secret %q survived: %q", secret, got)
				}
			}
			for _, context := range test.mustKeep {
				if !strings.Contains(got, context) {
					t.Errorf("useful context %q was lost: %q", context, got)
				}
			}
		})
	}
}

// TestRedactLineLeavesDiagnosticsAlone guards against over-redaction: lines a
// human needs to diagnose a crash must pass through untouched.
func TestRedactLineLeavesDiagnosticsAlone(t *testing.T) {
	lines := []string{
		`panic: runtime error: invalid memory address or nil pointer dereference`,
		`exec /app/server: no such file or directory`,
		`listening on http://0.0.0.0:8080`,
		`error: missing required env var DATABASE_URL`,
		`token bucket refilled, capacity 100`,
		`java.lang.OutOfMemoryError: Java heap space`,
		`passed 42 of 42 checks`,
		`pwd is /app/data`,
		`compass heading set`,
		`bypass cache for /healthz`,
		`tests passed: 12`,
		`Authorization header missing`,
		`--password-file /run/secrets/db`,
		`PASS: all 12 tests`,
		`tokens remaining: 42`,
		`Token bucket full`,
		`bearer token expired`,
	}
	for _, line := range lines {
		got, count := redactLine(line)
		if count != 0 || got != line {
			t.Errorf("redactLine(%q) = %q (%d redactions), want unchanged", line, got, count)
		}
	}
}

// TestRedactLineCountsEachValueOnce guards the redaction count against rules
// that overlap. "--token x" matches both the flag rule and the key name rule;
// it is one secret, and the count the agent sees must say one.
func TestRedactLineCountsEachValueOnce(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"--token abc123def456ghi789", 1},
		{"Authorization: Token abc123def456ghi789", 1},
		{"DB_PASSWORD=hunter2", 1},
		{"connecting postgres://app:hunter2@db/x DB_PASSWORD=hunter2", 2},
	}
	for _, test := range tests {
		if _, got := redactLine(test.line); got != test.want {
			t.Errorf("redactLine(%q) count = %d, want %d", test.line, got, test.want)
		}
	}
}

// fakeLeakPassword is planted in every free-text field the cluster can return.
// it is assembled at runtime for the same reason as the other fake values.
var fakeLeakPassword = "Leak" + "Pass" + "9271"

// leakText puts the password in two common shapes: a URL credential and a
// key=value pair.
func leakText() string {
	return "connecting postgres://app:" + fakeLeakPassword + "@db:5432/x; DB_PASSWORD=" + fakeLeakPassword
}

// assertNoLeak fails if the marshalled tool output contains the password
// anywhere. checking the serialised JSON rather than individual fields means a
// field added later is covered without updating this test.
func assertNoLeak(t *testing.T, output any) {
	t.Helper()
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshalling output: %v", err)
	}
	if strings.Contains(string(raw), fakeLeakPassword) {
		t.Errorf("password reached tool output:\n%s", raw)
	}
}

// TestExplainCrashloopRedactsEveryTextField covers the termination message,
// which the kubelet fills from the log tail under FallbackToLogsOnError, and
// the waiting message, which carries runtime errors.
func TestExplainCrashloopRedactsEveryTextField(t *testing.T) {
	status := corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 3,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerConfigError", Message: leakText(),
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", Message: leakText(),
		}},
	}
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"}, status)},
		CrashloopInput{Namespace: "default", Pod: "p"})
	assertNoLeak(t, out)
}

func TestExplainCrashloopRedactsCurrentTermination(t *testing.T) {
	status := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", Message: leakText(),
		}},
	}
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"}, status)},
		CrashloopInput{Namespace: "default", Pod: "p"})
	assertNoLeak(t, out)
}

// TestExplainCrashloopRedactsSchedulingMessage covers the pending path.
func TestExplainCrashloopRedactsSchedulingMessage(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: leakText(),
			}},
		},
	}
	out := explain(t, []*corev1.Pod{p}, CrashloopInput{Namespace: "default", Pod: "p"})
	assertNoLeak(t, out)
}

// TestGetPodEventsRedactsMessages covers probe output and exec errors, which
// arrive as event messages, through both events APIs.
func TestGetPodEventsRedactsMessages(t *testing.T) {
	objects := []runtime.Object{
		coreEvent("p", "Warning", "Unhealthy", "Readiness probe failed: "+leakText(), 1, time.Minute),
		&eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "default"},
			Regarding:  corev1.ObjectReference{Kind: "Pod", Name: "p", Namespace: "default"},
			Type:       "Warning", Reason: "Failed", Note: "exec failed: " + leakText(),
			EventTime: metav1.NewMicroTime(time.Now()),
		},
	}
	out := podEvents(t, objects, PodEventsInput{Namespace: "default", Pod: "p"})
	assertNoLeak(t, out)
	if len(out.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2: redaction must not drop events", len(out.Events))
	}
}

// TestGetWorkloadStatusRedactsConditionMessages covers all three workload kinds.
func TestGetWorkloadStatusRedactsConditionMessages(t *testing.T) {
	failing := func(message string) []appsv1.DeploymentCondition {
		return []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: "False",
			Reason: "ProgressDeadlineExceeded", Message: message,
		}}
	}
	objects := []runtime.Object{
		deployment("web", func(d *appsv1.Deployment) { d.Status.Conditions = failing(leakText()) }),
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 1},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr(1)},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Conditions: []appsv1.StatefulSetCondition{{
				Type: "Ready", Status: "False", Message: leakText(),
			}}},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", Generation: 1},
			Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, Conditions: []appsv1.DaemonSetCondition{{
				Type: "Ready", Status: "False", Message: leakText(),
			}}},
		},
	}
	clientset := fake.NewClientset(objects...)
	for _, name := range []string{"web", "db", "agent"} {
		out, err := GetWorkloadStatus(context.Background(), clientset, WorkloadStatusInput{Namespace: "default", Name: name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertNoLeak(t, out)
	}
}
