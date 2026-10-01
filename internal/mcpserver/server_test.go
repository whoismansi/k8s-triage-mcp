package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
