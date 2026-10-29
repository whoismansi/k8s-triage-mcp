// Package mcpserver adapts the triage package to the MCP protocol. It is
// deliberately thin: every tool here unpacks arguments, calls into triage, and
// returns the result struct. No diagnostic logic lives in this package.
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whoismansi/k8s-triage-mcp/internal/triage"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
)

// Version is the server version advertised to clients.
const Version = "v0.1.0"

// ServerName is the name advertised to clients during initialization.
const ServerName = "k8s-triage-mcp"

// toolTimeout bounds every tool call. the agent waits on the call, and an
// unreachable or overloaded API server would otherwise hold it until the
// client gives up, with no explanation. a timeout comes back as a normal tool
// error the agent can read and report.
var toolTimeout = 30 * time.Second

// bounded runs call with the tool timeout and returns as soon as either the
// call finishes or the timeout fires.
//
// client-go cancels an in-flight HTTP request when its context ends, so the
// context alone usually suffices. the select is the backstop for anything on
// the call path that does not honour the context: the agent gets an answer at
// the deadline regardless. the channel is buffered so a call that finishes
// after the deadline can still send and exit, rather than leaking a goroutine.
func bounded[T any](ctx context.Context, call func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call(ctx)
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("the Kubernetes API did not respond within %s: %w", toolTimeout, ctx.Err())
	}
}

// New builds an MCP server with every triage tool registered against the given
// clientset.
func New(clientset kubernetes.Interface) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: "Read-only diagnostics for unhealthy Kubernetes workloads. " +
			"Tools return structured findings with stable `code` values; reason over " +
			"the evidence fields rather than the message strings. This server never " +
			"mutates cluster state.",
	})

	registerHealthCheck(server, clientset)
	registerWorkloadStatus(server, clientset)
	registerExplainCrashloop(server, clientset)
	registerPodEvents(server, clientset)
	registerRevisionDiff(server, clientset)

	return server
}

// registerRevisionDiff exposes triage.DiffDeploymentRevisions.
func registerRevisionDiff(server *mcp.Server, clientset kubernetes.Interface) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "diff_deployment_revisions",
		Description: "Show what changed between two revisions of a Deployment's pod template: image, env vars, " +
			"resources, probes, command and args, and volumes, with other changed fields named. Defaults to the " +
			"latest revision versus the one before it, which answers \"it worked before the last deploy\". Env " +
			"values from Secrets and ConfigMaps are shown as references only, and literal values with secret " +
			"names are redacted. Lists the retained revisions so another pair can be requested.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triage.RevisionDiffInput) (*mcp.CallToolResult, triage.RevisionDiffOutput, error) {
		out, err := bounded(ctx, func(ctx context.Context) (triage.RevisionDiffOutput, error) {
			return triage.DiffDeploymentRevisions(ctx, clientset, in)
		})
		if err != nil {
			return nil, triage.RevisionDiffOutput{}, err
		}
		return nil, out, nil
	})
}

// registerPodEvents exposes triage.GetPodEvents.
func registerPodEvents(server *mcp.Server, clientset kubernetes.Interface) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_pod_events",
		Description: "Return recent Kubernetes events for one pod or every pod matching a label selector, newest " +
			"first, with repeats merged. Routine lifecycle events (Scheduled, Pulling, Pulled, Created, Started) " +
			"are hidden by default; the filtered count is reported so an empty result is never ambiguous. " +
			"Use this to find why a pod is failing to schedule, pull, mount or pass probes. Works for pods that " +
			"no longer exist, since events outlive them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triage.PodEventsInput) (*mcp.CallToolResult, triage.PodEventsOutput, error) {
		out, err := bounded(ctx, func(ctx context.Context) (triage.PodEventsOutput, error) {
			return triage.GetPodEvents(ctx, clientset, in)
		})
		if err != nil {
			return nil, triage.PodEventsOutput{}, err
		}
		return nil, out, nil
	})
}

// registerExplainCrashloop exposes triage.ExplainCrashloop.
func registerExplainCrashloop(server *mcp.Server, clientset kubernetes.Interface) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "explain_crashloop",
		Description: "Explain why a pod's containers are failing: per-container restart count, last exit code, " +
			"reason and signal, and the tail of the previous (crashed) container's log. Reports every container " +
			"including init containers unless one is named. Exit code 137 is reported as oom_killed only when " +
			"the kubelet says OOMKilled; otherwise it is killed_by_signal with hasLivenessProbe as evidence.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triage.CrashloopInput) (*mcp.CallToolResult, triage.CrashloopOutput, error) {
		out, err := bounded(ctx, func(ctx context.Context) (triage.CrashloopOutput, error) {
			return triage.ExplainCrashloop(ctx, clientset, in)
		})
		if err != nil {
			return nil, triage.CrashloopOutput{}, err
		}
		return nil, out, nil
	})
}

// registerWorkloadStatus exposes triage.GetWorkloadStatus.
//
// the handler calls into triage and returns the
// result. All diagnostic logic lives in the triage package, which keeps it
// testable without a protocol in the way.
func registerWorkloadStatus(server *mcp.Server, clientset kubernetes.Interface) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_workload_status",
		Description: "Report replica health and rollout state for a Deployment, StatefulSet or DaemonSet. " +
			"Use this first when asked why a workload is unhealthy: it says whether replicas are missing " +
			"and whether the rollout stalled, but not why. Follow up with get_pod_events or " +
			"explain_crashloop for the cause.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triage.WorkloadStatusInput) (*mcp.CallToolResult, triage.WorkloadStatusOutput, error) {
		out, err := bounded(ctx, func(ctx context.Context) (triage.WorkloadStatusOutput, error) {
			return triage.GetWorkloadStatus(ctx, clientset, in)
		})
		if err != nil {
			return nil, triage.WorkloadStatusOutput{}, err
		}
		return nil, out, nil
	})
}

// HealthCheckInput is the argument struct for the health_check tool.
type HealthCheckInput struct {
	// no arguments: this tool reports on the server's own connection.
}

// HealthCheckOutput reports whether the server can reach the cluster.
type HealthCheckOutput struct {
	Reachable     bool   `json:"reachable" jsonschema:"whether the Kubernetes API server responded"`
	ServerVersion string `json:"serverVersion,omitempty" jsonschema:"reported Kubernetes server version"`
	ReadOnly      bool   `json:"readOnly" jsonschema:"always true; this server issues no mutating API calls"`
}

// registerHealthCheck wires up a minimal tool that confirms cluster
// connectivity. It doubles as the canary for output-schema generation.
func registerHealthCheck(server *mcp.Server, clientset kubernetes.Interface) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "health_check",
		Description: "Verify the triage server can reach the Kubernetes API server.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ HealthCheckInput) (*mcp.CallToolResult, HealthCheckOutput, error) {
		versionInfo, err := bounded(ctx, func(context.Context) (*version.Info, error) {
			// discovery's ServerVersion takes no context; bounded still returns at the deadline.
			return clientset.Discovery().ServerVersion()
		})
		if err != nil {
			return nil, HealthCheckOutput{}, fmt.Errorf("cannot reach Kubernetes API server: %w", err)
		}
		return nil, HealthCheckOutput{
			Reachable:     true,
			ServerVersion: versionInfo.GitVersion,
			ReadOnly:      true,
		}, nil
	})
}
