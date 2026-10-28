package triage

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// log tail limits. every returned line lands in the agent's context window, so
// the default is sized for a typical stack trace plus some lead-in, and the cap
// stops a model that asks for "everything, to be safe" from crowding out the
// evidence returned by other tools.
const (
	// DefaultTailLines covers a typical stack trace plus a little lead-in.
	DefaultTailLines = 50
	// MaxTailLines bounds context usage when the agent asks for more.
	MaxTailLines = 200
	// MaxLogBytes is a backstop for pathological single lines (minified JSON,
	// base64 dumps) that a line count alone would not bound.
	MaxLogBytes = 64 * 1024
)

// CrashloopInput selects the pod to explain.
type CrashloopInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the pod"`
	Pod       string `json:"pod" jsonschema:"name of the pod"`
	// Container is optional. When empty, every container is reported, because
	// the agent usually does not know which one is failing.
	Container string `json:"container,omitempty" jsonschema:"optional container name; when omitted all containers including init containers are reported"`
	TailLines int    `json:"tailLines,omitempty" jsonschema:"log lines to return per container; default 50, values above 200 are clamped to 200"`
}

// TerminationInfo describes one container exit.
type TerminationInfo struct {
	ExitCode int32 `json:"exitCode" jsonschema:"process exit code"`
	Signal   int32 `json:"signal,omitempty" jsonschema:"signal number when the process was killed by a signal"`
	// SignalName is derived, e.g. SIGKILL for exit code 137.
	SignalName string `json:"signalName,omitempty" jsonschema:"signal name, e.g. SIGKILL"`
	Reason     string `json:"reason,omitempty" jsonschema:"kubelet reason, e.g. OOMKilled, Error, Completed, StartError"`
	Message    string `json:"message,omitempty" jsonschema:"kubelet or runtime message"`
	StartedAt  string `json:"startedAt,omitempty" jsonschema:"RFC3339 time the container started"`
	FinishedAt string `json:"finishedAt,omitempty" jsonschema:"RFC3339 time the container exited"`
	// RunSeconds separates instant startup failures from crashes after a long
	// run, which point at very different causes.
	RunSeconds *float64 `json:"runSeconds,omitempty" jsonschema:"seconds between start and exit"`
}

// LogTail is the tail of one container's log.
type LogTail struct {
	// Previous is true when the log came from the last terminated instance,
	// which is where the evidence for a crash usually is.
	Previous           bool     `json:"previous" jsonschema:"true when read from the previous (crashed) container instance"`
	RequestedTailLines int      `json:"requestedTailLines" jsonschema:"tail lines the caller asked for, 0 meaning default"`
	EffectiveTailLines int      `json:"effectiveTailLines" jsonschema:"tail lines actually requested after defaulting and clamping"`
	Lines              []string `json:"lines" jsonschema:"log lines, oldest first"`
	MayBeTruncated     bool     `json:"mayBeTruncated" jsonschema:"true when the line or byte limit was reached, so older output may exist"`
	// Redactions counts credential-shaped values replaced with [REDACTED]. it
	// is best effort, so zero means none were recognised, not that none exist.
	Redactions int `json:"redactions" jsonschema:"number of credential-shaped values replaced with [REDACTED]"`
	// Error is set when logs could not be read. A log failure does not fail the
	// tool, because container state alone is still useful evidence.
	Error string `json:"error,omitempty" jsonschema:"why logs could not be read, if they could not"`
}

// ContainerReport is the per-container state relevant to a crash.
type ContainerReport struct {
	Name          string `json:"name" jsonschema:"container name"`
	Init          bool   `json:"init" jsonschema:"true for init containers"`
	Image         string `json:"image" jsonschema:"container image"`
	Ready         bool   `json:"ready" jsonschema:"container readiness"`
	RestartCount  int32  `json:"restartCount" jsonschema:"number of restarts"`
	State         string `json:"state" jsonschema:"current state: running, waiting, terminated or unknown"`
	WaitingReason string `json:"waitingReason,omitempty" jsonschema:"reason when waiting, e.g. CrashLoopBackOff"`
	WaitingMsg    string `json:"waitingMessage,omitempty" jsonschema:"message when waiting"`
	HasLiveness   bool   `json:"hasLivenessProbe" jsonschema:"whether a liveness probe is configured; a liveness failure also ends in SIGKILL"`
	MemoryLimit   string `json:"memoryLimit,omitempty" jsonschema:"configured memory limit, e.g. 128Mi"`
	// LastTermination is the previous instance's exit: the crash itself.
	LastTermination *TerminationInfo `json:"lastTermination,omitempty" jsonschema:"exit details of the previous container instance"`
	// CurrentTermination is set when the container is terminated right now.
	CurrentTermination *TerminationInfo `json:"currentTermination,omitempty" jsonschema:"exit details when the container is currently terminated"`
	Logs               *LogTail         `json:"logs,omitempty" jsonschema:"tail of the relevant container log"`
}

// CrashloopOutput explains why a pod's containers are failing.
type CrashloopOutput struct {
	Object     ObjectRef         `json:"object" jsonschema:"the pod this report describes"`
	Phase      string            `json:"phase" jsonschema:"pod phase"`
	Node       string            `json:"node,omitempty" jsonschema:"node the pod is scheduled on"`
	Containers []ContainerReport `json:"containers" jsonschema:"per-container state, init containers first"`
	// Findings across all containers. Each carries a container key in its
	// evidence so the agent can tell them apart.
	Findings []Finding `json:"findings" jsonschema:"structured observations; each has evidence.container"`
}

// signalNames covers signals that realistically end a container.
var signalNames = map[int32]string{
	1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 6: "SIGABRT",
	7: "SIGBUS", 8: "SIGFPE", 9: "SIGKILL", 11: "SIGSEGV", 13: "SIGPIPE",
	15: "SIGTERM",
}

// ExplainCrashloop reports container exit state and previous logs for a pod.
func ExplainCrashloop(ctx context.Context, clientset kubernetes.Interface, in CrashloopInput) (CrashloopOutput, error) {
	pod, err := clientset.CoreV1().Pods(in.Namespace).Get(ctx, in.Pod, metav1.GetOptions{})
	if err != nil {
		return CrashloopOutput{}, fmt.Errorf("getting pod %s/%s: %w", in.Namespace, in.Pod, err)
	}

	out := CrashloopOutput{
		Object:     ObjectRef{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name},
		Phase:      string(pod.Status.Phase),
		Node:       pod.Spec.NodeName,
		Containers: []ContainerReport{},
		Findings:   []Finding{},
	}

	effectiveTail := clampTailLines(in.TailLines)

	type target struct {
		spec   corev1.Container
		status *corev1.ContainerStatus
		init   bool
	}
	var targets []target
	for _, spec := range pod.Spec.InitContainers {
		targets = append(targets, target{spec, findStatus(pod.Status.InitContainerStatuses, spec.Name), true})
	}
	for _, spec := range pod.Spec.Containers {
		targets = append(targets, target{spec, findStatus(pod.Status.ContainerStatuses, spec.Name), false})
	}

	if in.Container != "" {
		var filtered []target
		var names []string
		for _, t := range targets {
			names = append(names, t.spec.Name)
			if t.spec.Name == in.Container {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			return CrashloopOutput{}, fmt.Errorf("pod %s/%s has no container %q; containers are: %s",
				in.Namespace, in.Pod, in.Container, strings.Join(names, ", "))
		}
		targets = filtered
	}

	for _, t := range targets {
		report := buildReport(t.spec, t.status, t.init)
		if report.LastTermination != nil || report.CurrentTermination != nil {
			// when the container is terminated right now, the instance that
			// just died is the current one, so its own log holds the evidence.
			// only while it is waiting or running again does that evidence move
			// to the previous instance.
			previous := report.CurrentTermination == nil
			report.Logs = readLogs(ctx, clientset, pod, t.spec.Name, previous, in.TailLines, effectiveTail)
		}
		out.Containers = append(out.Containers, report)
		out.Findings = append(out.Findings, analyseContainer(report)...)
	}

	// a pod with no container statuses at all never started. that is a
	// scheduling or admission problem, not a crashloop, so say so explicitly
	// rather than returning an empty report the agent might read as healthy.
	if len(pod.Status.ContainerStatuses) == 0 && len(pod.Status.InitContainerStatuses) == 0 {
		finding := NewFinding(CodePodNotStarted, SeverityInfo,
			"no containers have started; this is not a crashloop").
			WithEvidence("phase", string(pod.Status.Phase))
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status != corev1.ConditionTrue {
				finding = finding.WithEvidence("scheduledReason", condition.Reason).
					WithEvidence("scheduledMessage", condition.Message)
			}
		}
		out.Findings = append(out.Findings, finding)
	}

	return out, nil
}

// clampTailLines applies the default and the ceiling. Out-of-range requests
// are clamped rather than rejected: an error costs the agent a round trip and
// teaches it nothing, while effectiveTailLines in the output shows the clamp.
func clampTailLines(requested int) int {
	if requested <= 0 {
		return DefaultTailLines
	}
	if requested > MaxTailLines {
		return MaxTailLines
	}
	return requested
}

func findStatus(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for index := range statuses {
		if statuses[index].Name == name {
			return &statuses[index]
		}
	}
	return nil
}

func buildReport(spec corev1.Container, status *corev1.ContainerStatus, init bool) ContainerReport {
	report := ContainerReport{
		Name:        spec.Name,
		Init:        init,
		Image:       spec.Image,
		State:       "unknown",
		HasLiveness: spec.LivenessProbe != nil,
	}
	if limit, ok := spec.Resources.Limits[corev1.ResourceMemory]; ok {
		report.MemoryLimit = limit.String()
	}
	if status == nil {
		return report
	}

	report.Ready = status.Ready
	report.RestartCount = status.RestartCount

	switch {
	case status.State.Running != nil:
		report.State = "running"
	case status.State.Waiting != nil:
		report.State = "waiting"
		report.WaitingReason = status.State.Waiting.Reason
		report.WaitingMsg = status.State.Waiting.Message
	case status.State.Terminated != nil:
		report.State = "terminated"
		report.CurrentTermination = terminationInfo(status.State.Terminated)
	}
	if status.LastTerminationState.Terminated != nil {
		report.LastTermination = terminationInfo(status.LastTerminationState.Terminated)
	}
	return report
}

func terminationInfo(terminated *corev1.ContainerStateTerminated) *TerminationInfo {
	info := &TerminationInfo{
		ExitCode:   terminated.ExitCode,
		Signal:     terminated.Signal,
		Reason:     terminated.Reason,
		Message:    terminated.Message,
		StartedAt:  formatTime(terminated.StartedAt),
		FinishedAt: formatTime(terminated.FinishedAt),
	}
	// the kubelet usually leaves Signal at 0 even for signal deaths, so derive
	// it from the shell convention: exit code 128+N means killed by signal N.
	if info.Signal == 0 && terminated.ExitCode > 128 && terminated.ExitCode < 160 {
		info.Signal = terminated.ExitCode - 128
	}
	if info.Signal != 0 {
		info.SignalName = signalNames[info.Signal]
	}
	if !terminated.StartedAt.IsZero() && !terminated.FinishedAt.IsZero() {
		seconds := terminated.FinishedAt.Sub(terminated.StartedAt.Time).Seconds()
		info.RunSeconds = &seconds
	}
	return info
}

// readLogs fetches the tail of a container log. Failure is recorded in the
// result instead of failing the tool, because exit state alone still helps.
func readLogs(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, container string, previous bool, requested, effective int) *LogTail {
	tail := &LogTail{
		Previous:           previous,
		RequestedTailLines: requested,
		EffectiveTailLines: effective,
		Lines:              []string{},
	}

	tailLines := int64(effective)
	limitBytes := int64(MaxLogBytes)
	raw, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container:  container,
		Previous:   previous,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).DoRaw(ctx)
	if err != nil {
		tail.Error = err.Error()
		return tail
	}

	// the kubelet reports some failures, such as logs that the container
	// runtime has already removed, as a successful response whose body is an
	// error message. passing that through would present it to the agent as
	// output from the application.
	if message, failed := kubeletLogError(raw); failed {
		tail.Error = message
		return tail
	}

	fillLogTail(tail, raw, effective)
	return tail
}

// kubeletLogError reports whether a log response body is a kubelet error
// message rather than container output.
func kubeletLogError(raw []byte) (string, bool) {
	text := strings.TrimSpace(string(raw))
	if strings.Contains(text, "\n") {
		return "", false
	}
	if strings.HasPrefix(text, "unable to retrieve container logs for ") ||
		strings.HasPrefix(text, "failed to try resolving symlinks in path") {
		return text, true
	}
	return "", false
}

// fillLogTail splits raw log output into lines, records truncation, and
// redacts credentials. it is separate from the fetch so it can be tested with
// real log content, which the fake clientset cannot return.
func fillLogTail(tail *LogTail, raw []byte, effective int) {
	text := strings.TrimRight(string(raw), "\n")
	if text != "" {
		tail.Lines = strings.Split(text, "\n")
	}
	tail.MayBeTruncated = len(tail.Lines) >= effective || len(raw) >= MaxLogBytes
	tail.Redactions = redactLines(tail.Lines)
}

// analyseContainer turns one container's state into findings. It reports
// facts and maps well-known exit codes, but does not speculate about causes
// the evidence cannot prove.
func analyseContainer(report ContainerReport) []Finding {
	var findings []Finding
	tag := func(finding Finding) Finding {
		return finding.WithEvidence("container", report.Name).WithEvidence("init", report.Init)
	}

	switch report.WaitingReason {
	case "CrashLoopBackOff":
		findings = append(findings, tag(NewFinding(CodeCrashLoopBackOff, SeverityCritical,
			"container is in CrashLoopBackOff").
			WithEvidence("restartCount", report.RestartCount)))
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		findings = append(findings, tag(NewFinding(CodeImagePullFailed, SeverityCritical,
			"container image cannot be pulled").
			WithEvidence("image", report.Image).
			WithEvidence("reason", report.WaitingReason).
			WithEvidence("message", report.WaitingMsg)))
	case "CreateContainerConfigError", "CreateContainerError":
		// typically a missing secret, configmap or key referenced by env.
		findings = append(findings, tag(NewFinding(CodeContainerConfigError, SeverityCritical,
			"container could not be created from its configuration").
			WithEvidence("reason", report.WaitingReason).
			WithEvidence("message", report.WaitingMsg)))
	case "RunContainerError":
		findings = append(findings, tag(NewFinding(CodeContainerStartFailed, SeverityCritical,
			"runtime failed to start the container").
			WithEvidence("message", report.WaitingMsg)))
	}

	// the most recent exit is the evidence. when the container is terminated
	// right now, that is the current instance; otherwise, during
	// CrashLoopBackOff or after a restart, it is the previous instance.
	exit := report.CurrentTermination
	if exit == nil {
		exit = report.LastTermination
	}
	if exit == nil {
		return findings
	}

	base := func(code string, severity Severity, message string) Finding {
		finding := NewFinding(code, severity, message).
			WithEvidence("exitCode", exit.ExitCode).
			WithEvidence("reason", exit.Reason).
			WithEvidence("restartCount", report.RestartCount)
		if exit.RunSeconds != nil {
			finding = finding.WithEvidence("runSeconds", *exit.RunSeconds)
		}
		return tag(finding)
	}

	switch {
	case exit.Reason == "OOMKilled":
		// the only case where oom_killed is reported. exit code 137 alone is
		// not proof of an OOM kill.
		findings = append(findings, base(CodeOOMKilled, SeverityCritical,
			"container was killed for exceeding its memory limit").
			WithEvidence("memoryLimit", report.MemoryLimit))

	case exit.Reason == "StartError" || exit.Reason == "ContainerCannotRun":
		// the runtime could not exec the entrypoint at all, e.g. a missing
		// binary under containerd surfaces here with exit code 128.
		findings = append(findings, base(CodeContainerStartFailed, SeverityCritical,
			"runtime could not start the container process").
			WithEvidence("message", exit.Message))

	case exit.ExitCode == 127:
		findings = append(findings, base(CodeCommandNotFound, SeverityCritical,
			"container command was not found (exit code 127)"))

	case exit.ExitCode == 126:
		findings = append(findings, base(CodeNonZeroExit, SeverityCritical,
			"container command exists but could not be executed (exit code 126)").
			WithEvidence("exitCodeMeaning", "command not executable"))

	case exit.ExitCode == 139:
		findings = append(findings, base(CodeSegfault, SeverityCritical,
			"container process crashed with a segmentation fault"))

	case exit.Signal != 0:
		// killed by a signal for a reason other than OOM. for SIGKILL this is
		// often a failed liveness probe or an exceeded termination grace
		// period; hasLivenessProbe is evidence for the model to weigh, not a
		// conclusion drawn here.
		severity := SeverityWarning
		if exit.Signal == 15 {
			// SIGTERM is usually a graceful stop requested by the kubelet.
			severity = SeverityInfo
		}
		findings = append(findings, base(CodeKilledBySignal, severity,
			"container was killed by a signal").
			WithEvidence("signal", exit.Signal).
			WithEvidence("signalName", exit.SignalName).
			WithEvidence("hasLivenessProbe", report.HasLiveness))

	case exit.ExitCode == 0:
		// a clean exit only matters if the container keeps getting restarted
		// (restartPolicy Always): the process is not long-running. a completed
		// init container is normal and not a finding.
		if report.RestartCount > 0 && !report.Init {
			findings = append(findings, base(CodeExitedCleanly, SeverityWarning,
				"container exits with code 0 and is restarted; its process is not long-running"))
		}

	default:
		findings = append(findings, base(CodeNonZeroExit, SeverityCritical,
			"container exited with a non-zero code"))
	}

	return findings
}
