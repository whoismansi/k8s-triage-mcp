// Package mcpserver adapts the triage package to the MCP protocol. It is
// deliberately thin: every tool here unpacks arguments, calls into triage, and
// returns the result struct. No diagnostic logic lives in this package.
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
)

// Version is the server version advertised to clients.
const Version = "v0.1.0"

// ServerName is the name advertised to clients during initialization.
const ServerName = "k8s-triage-mcp"

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

	return server
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
		versionInfo, err := clientset.Discovery().ServerVersion()
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
