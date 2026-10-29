package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// twoContexts is a kubeconfig with two clusters so tests can tell which one
// was selected. no credentials are needed: New never contacts the cluster.
const twoContexts = `apiVersion: v1
kind: Config
current-context: first
clusters:
- name: first
  cluster:
    server: https://first.example:6443
- name: second
  cluster:
    server: https://second.example:6443
users:
- name: someone
  user: {}
contexts:
- name: first
  context: {cluster: first, user: someone}
- name: second
  context: {cluster: second, user: someone}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(twoContexts), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewSelectsContext(t *testing.T) {
	path := writeKubeconfig(t)
	tests := []struct {
		name        string
		context     string
		wantContext string
		wantHost    string
	}{
		{"current-context by default", "", "first", "https://first.example:6443"},
		{"explicit context wins", "second", "second", "https://second.example:6443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(Options{KubeconfigPath: path, Context: test.context})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if client.ContextName != test.wantContext || client.Host != test.wantHost {
				t.Errorf("got context %q host %q, want %q %q", client.ContextName, client.Host, test.wantContext, test.wantHost)
			}
		})
	}
}

// TestNewRejectsUnknownContext: a typo in --context must fail loudly rather
// than silently fall back to the current context, which may be production.
func TestNewRejectsUnknownContext(t *testing.T) {
	_, err := New(Options{KubeconfigPath: writeKubeconfig(t), Context: "typo"})
	if err == nil {
		t.Fatal("New() error = nil, want an error for a context that does not exist")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error %q does not name the missing context", err)
	}
}

func TestNewRejectsMissingKubeconfig(t *testing.T) {
	_, err := New(Options{KubeconfigPath: filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("New() error = nil, want an error for a kubeconfig that does not exist")
	}
}

func TestReadOnlyUserAgent(t *testing.T) {
	// the user agent lives on the rest config, which New does not expose, so
	// check the helper directly.
	config := &rest.Config{}
	applyReadOnlyDefaults(config)
	if !strings.Contains(config.UserAgent, "read-only") {
		t.Errorf("UserAgent = %q, want it to identify the server as read-only", config.UserAgent)
	}
}
