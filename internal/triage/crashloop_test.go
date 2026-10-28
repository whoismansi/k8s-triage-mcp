package triage

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// crashedStatus builds a container status that is in CrashLoopBackOff with the
// given previous exit.
func crashedStatus(name string, exitCode int32, reason string, runFor time.Duration) corev1.ContainerStatus {
	finished := time.Now().Add(-30 * time.Second)
	return corev1.ContainerStatus{
		Name:         name,
		RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff",
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   exitCode,
			Reason:     reason,
			StartedAt:  metav1.NewTime(finished.Add(-runFor)),
			FinishedAt: metav1.NewTime(finished),
		}},
	}
}

// pod builds a single-container pod with the given status.
func pod(name string, spec corev1.Container, status corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{spec}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{status},
		},
	}
}

func explain(t *testing.T, objects []*corev1.Pod, in CrashloopInput) CrashloopOutput {
	t.Helper()
	clientset := fake.NewClientset()
	for _, object := range objects {
		if _, err := clientset.CoreV1().Pods(object.Namespace).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seeding pod: %v", err)
		}
	}
	out, err := ExplainCrashloop(context.Background(), clientset, in)
	if err != nil {
		t.Fatalf("ExplainCrashloop() error = %v", err)
	}
	return out
}

// TestExitCodeMapping is the heart of this tool: each well-known exit maps to
// exactly the right finding, and nothing more confident than the evidence.
func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		name       string
		container  corev1.Container
		exitCode   int32
		reason     string
		wantCode   string
		wantAbsent []string
	}{
		{
			name:      "exit 127 is command not found",
			container: corev1.Container{Name: "app", Image: "busybox"},
			exitCode:  127, reason: "Error",
			wantCode: CodeCommandNotFound,
		},
		{
			name:      "OOMKilled reason is oom_killed",
			container: corev1.Container{Name: "app", Image: "app:v1"},
			exitCode:  137, reason: "OOMKilled",
			wantCode:   CodeOOMKilled,
			wantAbsent: []string{CodeKilledBySignal},
		},
		{
			// the trap this tool exists to avoid: 137 without OOMKilled is a
			// SIGKILL from something else, usually a failed liveness probe.
			name: "exit 137 without OOMKilled is NOT reported as oom",
			container: corev1.Container{Name: "app", Image: "app:v1",
				LivenessProbe: &corev1.Probe{}},
			exitCode: 137, reason: "Error",
			wantCode:   CodeKilledBySignal,
			wantAbsent: []string{CodeOOMKilled},
		},
		{
			name:      "exit 139 is segfault",
			container: corev1.Container{Name: "app", Image: "app:v1"},
			exitCode:  139, reason: "Error",
			wantCode:   CodeSegfault,
			wantAbsent: []string{CodeKilledBySignal},
		},
		{
			name:      "exit 1 is a generic non-zero exit",
			container: corev1.Container{Name: "app", Image: "app:v1"},
			exitCode:  1, reason: "Error",
			wantCode: CodeNonZeroExit,
		},
		{
			name:      "exit 0 with restarts means the process is not long-running",
			container: corev1.Container{Name: "app", Image: "app:v1"},
			exitCode:  0, reason: "Completed",
			wantCode: CodeExitedCleanly,
		},
		{
			name:      "StartError is a start failure, not an app crash",
			container: corev1.Container{Name: "app", Image: "app:v1"},
			exitCode:  128, reason: "StartError",
			wantCode:   CodeContainerStartFailed,
			wantAbsent: []string{CodeNonZeroExit},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := crashedStatus(test.container.Name, test.exitCode, test.reason, 2*time.Second)
			out := explain(t, []*corev1.Pod{pod("p", test.container, status)},
				CrashloopInput{Namespace: "default", Pod: "p"})

			if !hasFinding(out.Findings, test.wantCode) {
				t.Errorf("missing finding %q; got %+v", test.wantCode, out.Findings)
			}
			for _, code := range test.wantAbsent {
				if hasFinding(out.Findings, code) {
					t.Errorf("unexpected finding %q; got %+v", code, out.Findings)
				}
			}
			// every crashed container in these cases is also in backoff.
			if !hasFinding(out.Findings, CodeCrashLoopBackOff) {
				t.Errorf("missing crashloop_backoff; got %+v", out.Findings)
			}
		})
	}
}

// TestSigkillCarriesLivenessEvidence checks that the server hands the model
// the fact it needs (a liveness probe exists) without drawing the conclusion.
func TestSigkillCarriesLivenessEvidence(t *testing.T) {
	container := corev1.Container{Name: "app", Image: "app:v1", LivenessProbe: &corev1.Probe{}}
	out := explain(t, []*corev1.Pod{pod("p", container, crashedStatus("app", 137, "Error", time.Minute))},
		CrashloopInput{Namespace: "default", Pod: "p"})

	finding := findingByCode(t, out.Findings, CodeKilledBySignal)
	if finding.Evidence["signalName"] != "SIGKILL" {
		t.Errorf("signalName = %v, want SIGKILL derived from exit code 137", finding.Evidence["signalName"])
	}
	if finding.Evidence["hasLivenessProbe"] != true {
		t.Errorf("hasLivenessProbe = %v, want true", finding.Evidence["hasLivenessProbe"])
	}
}

func TestOOMKilledCarriesMemoryLimit(t *testing.T) {
	container := corev1.Container{
		Name: "app", Image: "app:v1",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		}},
	}
	out := explain(t, []*corev1.Pod{pod("p", container, crashedStatus("app", 137, "OOMKilled", 5*time.Second))},
		CrashloopInput{Namespace: "default", Pod: "p"})

	finding := findingByCode(t, out.Findings, CodeOOMKilled)
	if finding.Evidence["memoryLimit"] != "32Mi" {
		t.Errorf("memoryLimit = %v, want 32Mi", finding.Evidence["memoryLimit"])
	}
}

func TestRunSecondsIsComputed(t *testing.T) {
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"},
		crashedStatus("app", 1, "Error", 3*time.Second))},
		CrashloopInput{Namespace: "default", Pod: "p"})

	exit := out.Containers[0].LastTermination
	if exit == nil || exit.RunSeconds == nil {
		t.Fatalf("LastTermination.RunSeconds not set: %+v", exit)
	}
	if *exit.RunSeconds < 2.9 || *exit.RunSeconds > 3.1 {
		t.Errorf("RunSeconds = %v, want about 3", *exit.RunSeconds)
	}
}

// TestPreviousLogsAreRead verifies logs are attached and taken from the
// previous instance. The fake clientset ignores log options and always returns
// "fake logs", so real content is verified against kind in phase 3.
func TestPreviousLogsAreRead(t *testing.T) {
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"},
		crashedStatus("app", 1, "Error", time.Second))},
		CrashloopInput{Namespace: "default", Pod: "p"})

	logs := out.Containers[0].Logs
	if logs == nil {
		t.Fatal("Logs = nil for a crashed container")
	}
	if !logs.Previous {
		t.Error("Logs.Previous = false; crash evidence lives in the previous instance")
	}
	if len(logs.Lines) == 0 {
		t.Error("Logs.Lines is empty")
	}
}

func TestHealthyContainerHasNoFindingsOrLogs(t *testing.T) {
	status := corev1.ContainerStatus{
		Name: "app", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"}, status)},
		CrashloopInput{Namespace: "default", Pod: "p"})

	if len(out.Findings) != 0 {
		t.Errorf("Findings = %+v, want none for a running container that never crashed", out.Findings)
	}
	if out.Containers[0].Logs != nil {
		t.Error("Logs fetched for a container that never terminated; that wastes context")
	}
}

func TestClampTailLines(t *testing.T) {
	tests := []struct{ requested, want int }{
		{0, DefaultTailLines},
		{-5, DefaultTailLines},
		{10, 10},
		{200, 200},
		{1000, MaxTailLines},
	}
	for _, test := range tests {
		if got := clampTailLines(test.requested); got != test.want {
			t.Errorf("clampTailLines(%d) = %d, want %d", test.requested, got, test.want)
		}
	}
}

func TestEffectiveTailLinesReportsClamp(t *testing.T) {
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"},
		crashedStatus("app", 1, "Error", time.Second))},
		CrashloopInput{Namespace: "default", Pod: "p", TailLines: 5000})

	logs := out.Containers[0].Logs
	if logs.RequestedTailLines != 5000 || logs.EffectiveTailLines != MaxTailLines {
		t.Errorf("requested=%d effective=%d, want 5000 and %d", logs.RequestedTailLines, logs.EffectiveTailLines, MaxTailLines)
	}
}

// TestMultiContainerReportsAll covers the decision to report every container:
// the sidecar is fine, the app is crashing, and both appear, init first.
func TestMultiContainerReportsAll(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "migrate"}},
			Containers:     []corev1.Container{{Name: "app"}, {Name: "proxy"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "migrate",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Reason: "Completed",
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				crashedStatus("app", 1, "Error", time.Second),
				{Name: "proxy", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	out := explain(t, []*corev1.Pod{p}, CrashloopInput{Namespace: "default", Pod: "p"})

	if len(out.Containers) != 3 {
		t.Fatalf("len(Containers) = %d, want 3", len(out.Containers))
	}
	if !out.Containers[0].Init || out.Containers[0].Name != "migrate" {
		t.Errorf("Containers[0] = %+v, want init container migrate first", out.Containers[0])
	}
	for _, finding := range out.Findings {
		if finding.Evidence["container"] != "app" {
			t.Errorf("finding %q attributed to %v; only app is failing", finding.Code, finding.Evidence["container"])
		}
	}
	// a completed init container is normal and must not raise exited_cleanly.
	if hasFinding(out.Findings, CodeExitedCleanly) {
		t.Error("completed init container raised exited_cleanly")
	}
}

func TestContainerFilter(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "proxy"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			crashedStatus("app", 1, "Error", time.Second),
			crashedStatus("proxy", 1, "Error", time.Second),
		}},
	}
	out := explain(t, []*corev1.Pod{p}, CrashloopInput{Namespace: "default", Pod: "p", Container: "proxy"})
	if len(out.Containers) != 1 || out.Containers[0].Name != "proxy" {
		t.Errorf("Containers = %+v, want only proxy", out.Containers)
	}
}

// TestUnknownContainerListsValidNames checks the error is written for a model:
// it names the valid choices so the agent can retry correctly in one step.
func TestUnknownContainerListsValidNames(t *testing.T) {
	clientset := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "proxy"}}},
	})
	_, err := ExplainCrashloop(context.Background(), clientset,
		CrashloopInput{Namespace: "default", Pod: "p", Container: "nope"})
	if err == nil {
		t.Fatal("error = nil, want an error for an unknown container")
	}
	for _, name := range []string{"app", "proxy"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not list valid container %q", err, name)
		}
	}
}

func TestImagePullAndConfigErrors(t *testing.T) {
	tests := []struct {
		waitingReason string
		wantCode      string
	}{
		{"ImagePullBackOff", CodeImagePullFailed},
		{"ErrImagePull", CodeImagePullFailed},
		{"CreateContainerConfigError", CodeContainerConfigError},
	}
	for _, test := range tests {
		t.Run(test.waitingReason, func(t *testing.T) {
			status := corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: test.waitingReason, Message: "detail"},
			}}
			out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app", Image: "app:typo"}, status)},
				CrashloopInput{Namespace: "default", Pod: "p"})
			if !hasFinding(out.Findings, test.wantCode) {
				t.Errorf("missing %q; got %+v", test.wantCode, out.Findings)
			}
		})
	}
}

func TestPendingPodIsNotACrashloop(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: "0/1 nodes are available: insufficient cpu.",
			}},
		},
	}
	out := explain(t, []*corev1.Pod{p}, CrashloopInput{Namespace: "default", Pod: "p"})

	finding := findingByCode(t, out.Findings, CodePodNotStarted)
	if finding.Evidence["scheduledReason"] != "Unschedulable" {
		t.Errorf("scheduledReason = %v, want Unschedulable", finding.Evidence["scheduledReason"])
	}
}

func TestMissingPodIsAnError(t *testing.T) {
	_, err := ExplainCrashloop(context.Background(), fake.NewClientset(),
		CrashloopInput{Namespace: "default", Pod: "nope"})
	if err == nil {
		t.Fatal("error = nil, want an error for a pod that does not exist")
	}
}

// TestLogTailIsRedacted pins that log lines pass through redaction before
// they are returned. it calls fillLogTail directly because the fake clientset
// always returns "fake logs" and cannot carry a credential.
func TestLogTailIsRedacted(t *testing.T) {
	raw := []byte("starting\nconnecting to postgres://app:" + "hunter2" + "@db:5432/orders\nDB_PASSWORD=" + "hunter2" + "\n")
	tail := &LogTail{Lines: []string{}}
	fillLogTail(tail, raw, DefaultTailLines)

	if len(tail.Lines) != 3 {
		t.Fatalf("len(Lines) = %d, want 3", len(tail.Lines))
	}
	for _, line := range tail.Lines {
		if strings.Contains(line, "hunter2") {
			t.Errorf("credential survived in log line %q", line)
		}
	}
	if tail.Redactions != 2 {
		t.Errorf("Redactions = %d, want 2", tail.Redactions)
	}
	if tail.MayBeTruncated {
		t.Error("MayBeTruncated = true for 3 lines under a 50 line limit")
	}
}

func TestLogTailTruncationFlag(t *testing.T) {
	tail := &LogTail{Lines: []string{}}
	fillLogTail(tail, []byte("a\nb\nc\n"), 3)
	if !tail.MayBeTruncated {
		t.Error("MayBeTruncated = false when the line limit was reached")
	}
}

// TestCurrentlyTerminatedUsesCurrentExitAndLog reproduces what a real kubelet
// reports between restarts: the container is terminated right now and also has
// an older termination. The exit that matters is the current one, and its log
// is the current log, not the previous one. Found against a kind cluster; the
// fake clientset had never produced this state.
func TestCurrentlyTerminatedUsesCurrentExitAndLog(t *testing.T) {
	now := time.Now()
	status := corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 5,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 127, Reason: "Error",
			StartedAt: metav1.NewTime(now.Add(-time.Second)), FinishedAt: metav1.NewTime(now),
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error",
			StartedAt: metav1.NewTime(now.Add(-time.Minute)), FinishedAt: metav1.NewTime(now.Add(-time.Minute)),
		}},
	}
	out := explain(t, []*corev1.Pod{pod("p", corev1.Container{Name: "app"}, status)},
		CrashloopInput{Namespace: "default", Pod: "p"})

	if !hasFinding(out.Findings, CodeCommandNotFound) {
		t.Errorf("findings = %+v, want command_not_found from the current exit 127", out.Findings)
	}
	if hasFinding(out.Findings, CodeNonZeroExit) {
		t.Errorf("findings = %+v, reported the older exit 1 instead of the current one", out.Findings)
	}
	if out.Containers[0].Logs.Previous {
		t.Error("Logs.Previous = true; a currently terminated container's evidence is its current log")
	}
}

// TestKubeletLogErrorIsNotTreatedAsOutput: the kubelet can return an error
// message as a successful log response. It must be reported as an error, not
// shown to the agent as application output.
func TestKubeletLogErrorIsNotTreatedAsOutput(t *testing.T) {
	tests := []struct {
		body       string
		wantFailed bool
	}{
		{"unable to retrieve container logs for containerd://4acd8a15a7ca", true},
		{"unable to retrieve container logs for containerd://4acd8a15a7ca\n", true},
		{"starting worker\nsh: run-worker: not found", false},
		{"unable to retrieve container logs for x\nbut this is real output", false},
		{"panic: unable to retrieve config", false},
	}
	for _, test := range tests {
		_, failed := kubeletLogError([]byte(test.body))
		if failed != test.wantFailed {
			t.Errorf("kubeletLogError(%q) = %v, want %v", test.body, failed, test.wantFailed)
		}
	}
}
