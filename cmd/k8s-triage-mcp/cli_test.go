package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitClusterFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCluster []string
		wantRest    []string
	}{
		{
			name:     "no cluster flags",
			args:     []string{"-n", "demo", "orders"},
			wantRest: []string{"-n", "demo", "orders"},
		},
		{
			name:        "separate values, anywhere in the line",
			args:        []string{"-n", "demo", "--context", "kind-x", "orders", "--kubeconfig", "/tmp/k"},
			wantCluster: []string{"--context", "kind-x", "--kubeconfig", "/tmp/k"},
			wantRest:    []string{"-n", "demo", "orders"},
		},
		{
			name:        "equals form and single dash",
			args:        []string{"--context=kind-x", "-kubeconfig=/tmp/k", "orders"},
			wantCluster: []string{"--context=kind-x", "-kubeconfig=/tmp/k"},
			wantRest:    []string{"orders"},
		},
		{
			// the dangling flag is passed on so the flag parser reports it,
			// rather than the pod name being swallowed as its value.
			name:        "trailing flag without a value",
			args:        []string{"orders", "--context"},
			wantCluster: []string{"--context"},
			wantRest:    []string{"orders"},
		},
		{
			name:     "similar names are not cluster flags",
			args:     []string{"--contexts", "--kubeconfig-dir=x"},
			wantRest: []string{"--contexts", "--kubeconfig-dir=x"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cluster, rest := splitClusterFlags(test.args)
			if !reflect.DeepEqual(cluster, test.wantCluster) || !reflect.DeepEqual(rest, test.wantRest) {
				t.Errorf("splitClusterFlags(%q) = %q, %q; want %q, %q", test.args, cluster, rest, test.wantCluster, test.wantRest)
			}
		})
	}
}

func TestFindCommand(t *testing.T) {
	for _, name := range []string{"status", "crashloop", "events", "diff"} {
		if _, ok := findCommand(name); !ok {
			t.Errorf("findCommand(%q) not found", name)
		}
	}
	if _, ok := findCommand("delete"); ok {
		t.Error(`findCommand("delete") found, want unknown commands rejected`)
	}
}

// TestRunCLIRejectsBadArguments: a failed command exits non-zero, writes
// nothing to stdout (callers pipe it into jq), and names itself on stderr.
func TestRunCLIRejectsBadArguments(t *testing.T) {
	// a valid kubeconfig pointing at an unroutable server: the missing name
	// must be caught before any request is attempted.
	kubeconfig := filepath.Join(t.TempDir(), "config")
	contents := "apiVersion: v1\nkind: Config\ncurrent-context: c\n" +
		"clusters: [{name: c, cluster: {server: \"https://127.0.0.1:1\"}}]\n" +
		"users: [{name: u, user: {}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\n"
	if err := os.WriteFile(kubeconfig, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, _ := findCommand("status")
	var stdout, stderr bytes.Buffer
	code := runCLI(cmd, []string{"--kubeconfig", kubeconfig, "-n", "demo"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("runCLI() = 0, want non-zero; stderr: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing written on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "status:") || !strings.Contains(stderr.String(), "workload name") {
		t.Errorf("stderr = %q, want the command and the missing argument named", stderr.String())
	}
}
