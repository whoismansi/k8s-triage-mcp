package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// connect wires a client to a server over an in-memory transport and returns
// the client session, cleaning both up when the test ends.
func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

// TestToolsAdvertiseStructuredSchemas is the load-bearing test for the whole
// design: it proves AddTool derives a real output schema from a concrete Out
// struct. If this fails, every tool must declare its schema by hand.
func TestToolsAdvertiseStructuredSchemas(t *testing.T) {
	session := connect(t, New(fake.NewClientset()))

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatal("server advertised no tools")
	}

	for _, tool := range result.Tools {
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the agent needs it to choose correctly", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %q has no output schema: structured output is not being generated", tool.Name)
			continue
		}

		schema, encoded, err := decodeSchema(tool.OutputSchema)
		if err != nil {
			t.Errorf("tool %q: %v", tool.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("tool %q output schema type = %q, want object", tool.Name, schema.Type)
		}
		if len(schema.Properties) == 0 {
			t.Errorf("tool %q output schema has no properties: fields were not inferred from the Out struct", tool.Name)
		}
		// printed so a human can confirm field names and jsonschema descriptions
		// survived inference, which assertions alone would not show.
		t.Logf("tool %q output schema: %s", tool.Name, encoded)
	}
}

// jsonSchema is the subset of JSON Schema these tests assert on.
type jsonSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// decodeSchema reinterprets a schema field as JSON. The MCP Tool type declares
// InputSchema and OutputSchema as `any` because a server may supply any value
// that marshals to valid JSON Schema, and clients receive it as a plain
// map[string]any. So there is no typed struct to assert against: round-tripping
// through JSON is the supported way to inspect it.
func decodeSchema(raw any) (jsonSchema, []byte, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return jsonSchema{}, nil, fmt.Errorf("marshalling schema: %w", err)
	}
	var schema jsonSchema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return jsonSchema{}, encoded, fmt.Errorf("schema is not a JSON object: %w", err)
	}
	return schema, encoded, nil
}

// TestHealthCheckReturnsStructuredContent verifies a tool call populates
// StructuredContent from the typed Out value rather than only text content.
func TestHealthCheckReturnsStructuredContent(t *testing.T) {
	session := connect(t, New(fake.NewClientset()))

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "health_check",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("calling health_check: %v", err)
	}
	if result.IsError {
		t.Fatalf("health_check reported an error: %+v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("no structured content: typed output is not reaching the client")
	}

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling structured content: %v", err)
	}
	var output HealthCheckOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("structured content does not match HealthCheckOutput: %v", err)
	}
	if !output.Reachable {
		t.Error("Reachable = false, want true against a fake clientset")
	}
	if !output.ReadOnly {
		t.Error("ReadOnly = false; this server must always report read-only")
	}
}

// TestEveryToolIsRegistered pins the public tool surface. renaming a tool
// breaks every client config and prompt that refers to it, so a change here
// should be deliberate.
func TestEveryToolIsRegistered(t *testing.T) {
	session := connect(t, New(fake.NewClientset()))
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range result.Tools {
		registered[tool.Name] = true
	}
	for _, name := range []string{
		"health_check", "get_workload_status", "explain_crashloop",
		"get_pod_events", "diff_deployment_revisions",
	} {
		if !registered[name] {
			t.Errorf("tool %q is not registered", name)
		}
	}
}

// TestToolsEndToEnd calls each tool through the protocol against one seeded
// cluster, proving arguments decode, handlers run and structured output comes
// back. tool logic is covered in the triage package; this checks the wiring.
func TestToolsEndToEnd(t *testing.T) {
	labels := map[string]string{"app": "api"}
	replicas := int32(1)
	clientset := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1},
		},
		replicaSet("api-1", "1", "api:1"),
		replicaSet("api-2", "2", "api:2"),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-pod", Namespace: "default", Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "api:2"}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "api",
				RestartCount: 3,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 127, Reason: "Error",
				}},
			}}},
		},
	)
	session := connect(t, New(clientset))

	tests := []struct {
		tool      string
		arguments map[string]any
		wantCode  string
	}{
		{"get_workload_status", map[string]any{"namespace": "default", "name": "api"}, "replicas_unavailable"},
		{"explain_crashloop", map[string]any{"namespace": "default", "pod": "api-pod"}, "command_not_found"},
		{"get_pod_events", map[string]any{"namespace": "default", "pod": "api-pod"}, ""},
		{"diff_deployment_revisions", map[string]any{"namespace": "default", "name": "api"}, "image_changed"},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: test.tool, Arguments: test.arguments})
			if err != nil {
				t.Fatalf("protocol error: %v", err)
			}
			if result.IsError {
				t.Fatalf("tool error: %+v", result.Content)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatalf("marshalling structured content: %v", err)
			}
			var output struct {
				Findings []struct {
					Code string `json:"code"`
				} `json:"findings"`
			}
			if err := json.Unmarshal(encoded, &output); err != nil {
				t.Fatalf("decoding structured content: %v", err)
			}
			if output.Findings == nil {
				t.Error("findings is null; it must always be an array")
			}
			if test.wantCode == "" {
				return
			}
			for _, finding := range output.Findings {
				if finding.Code == test.wantCode {
					return
				}
			}
			t.Errorf("no %q finding in %s", test.wantCode, encoded)
		})
	}
}

// TestToolErrorsAreToolResults checks a bad request comes back as a tool error
// the agent can read and correct, not a protocol failure that ends the session.
func TestToolErrorsAreToolResults(t *testing.T) {
	session := connect(t, New(fake.NewClientset()))
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_workload_status",
		Arguments: map[string]any{"namespace": "default", "name": "missing"},
	})
	if err != nil {
		t.Fatalf("protocol error = %v, want a tool error result instead", err)
	}
	if !result.IsError {
		t.Error("IsError = false for a workload that does not exist")
	}
}

func replicaSet(name, revision, image string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels:          map[string]string{"app": "api"},
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": revision},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "api", Image: image}},
		}}},
	}
}
