// Package triage contains the diagnostic logic for inspecting unhealthy
// Kubernetes workloads. It deliberately has no dependency on the MCP SDK: every
// function takes a kubernetes.Interface and returns plain structs, so the logic
// is testable against a fake clientset and reusable outside an MCP context.
package triage

import "fmt"

// Severity ranks a finding. The agent uses this to decide what to lead with
// when several findings come back from a single tool call.
type Severity string

const (
	// SeverityCritical means the workload is broken right now.
	SeverityCritical Severity = "critical"
	// SeverityWarning means something is wrong but the workload may still serve.
	SeverityWarning Severity = "warning"
	// SeverityInfo is context that helps explain a failure but is not itself one.
	SeverityInfo Severity = "info"
)

// Finding codes are stable API surface. The agent keys on these strings, and so
// do downstream tools, so they are documented in the README and must not be
// renamed casually. Add new codes rather than repurposing existing ones.
const (
	CodeImagePullFailed     = "image_pull_failed"
	CodeCrashLoopBackOff    = "crashloop_backoff"
	CodeCommandNotFound     = "command_not_found"
	CodeOOMKilled           = "oom_killed"
	CodeSegfault            = "segfault"
	CodeNonZeroExit         = "non_zero_exit"
	CodeProbeFailing        = "probe_failing"
	CodeUnschedulable       = "unschedulable"
	CodeProgressDeadline    = "progress_deadline_exceeded"
	CodeReplicasUnavailable = "replicas_unavailable"
	CodeRolloutStuck        = "rollout_stuck"
	CodeImageChanged        = "image_changed"
	CodeEnvChanged          = "env_changed"
	CodeResourcesChanged    = "resources_changed"
	CodeProbeChanged        = "probe_changed"
	CodeCommandChanged      = "command_changed"
	CodeOtherSpecChanged    = "other_spec_changed"

	CodeKilledBySignal       = "killed_by_signal"
	CodeExitedCleanly        = "exited_cleanly"
	CodeContainerStartFailed = "container_start_failed"
	CodeContainerConfigError = "container_config_error"
	CodePodNotStarted        = "pod_not_started"

	CodeVolumeMountFailed = "volume_mount_failed"
	CodeEvicted           = "evicted"
	CodePodCreateFailed   = "pod_create_failed"

	CodeRolloutIncomplete = "rollout_incomplete"
)

// Finding is a single structured diagnostic observation.
//
// The design rule for this struct: it carries facts, not conclusions written in
// prose. Message is a terse human-readable label, but Evidence is what the model
// actually reasons over, and every number a human would need to judge the
// finding belongs in Evidence rather than being interpolated into Message.
type Finding struct {
	// Code is the stable machine-readable identifier for this finding type.
	Code string `json:"code" jsonschema:"stable machine-readable finding identifier, e.g. oom_killed"`
	// Severity ranks how urgent this finding is.
	Severity Severity `json:"severity" jsonschema:"one of critical, warning, info"`
	// Message is a short factual description. Not an explanation or a suggestion.
	Message string `json:"message" jsonschema:"short factual description of the finding"`
	// Evidence holds the raw values supporting the finding, such as exit codes,
	// byte counts, or timestamps.
	Evidence map[string]any `json:"evidence,omitempty" jsonschema:"raw values supporting this finding"`
}

// NewFinding builds a Finding with no evidence attached.
func NewFinding(code string, severity Severity, message string) Finding {
	return Finding{Code: code, Severity: severity, Message: message}
}

// WithEvidence returns a copy of the finding with the given key set in its
// evidence map, allocating the map if needed. Chainable.
func (f Finding) WithEvidence(key string, value any) Finding {
	evidence := make(map[string]any, len(f.Evidence)+1)
	for k, v := range f.Evidence {
		evidence[k] = v
	}
	evidence[key] = value
	f.Evidence = evidence
	return f
}

// ObjectRef identifies the Kubernetes object a result describes. Every tool
// output embeds one so results stay self-describing when the agent holds
// several of them at once.
type ObjectRef struct {
	Kind      string `json:"kind" jsonschema:"object kind, e.g. Deployment or Pod"`
	Namespace string `json:"namespace" jsonschema:"object namespace"`
	Name      string `json:"name" jsonschema:"object name"`
}

// requireName reports an error naming the field when a required identifier is
// empty.
//
// namespace needs this in particular. the schema marks it required, but an
// empty string satisfies "required", and client-go treats namespace "" as all
// namespaces. without this check, a call meant for one namespace silently
// searches the whole cluster and returns events and pods the caller never
// asked about.
func requireName(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required and must not be empty", field)
	}
	return nil
}
