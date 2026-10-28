package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/whoismansi/k8s-triage-mcp/internal/kube"
	"github.com/whoismansi/k8s-triage-mcp/internal/triage"
	"k8s.io/client-go/kubernetes"
)

// cliTimeout bounds a single CLI call so a hung API server does not hang a
// demo script.
const cliTimeout = 30 * time.Second

// command is one CLI subcommand. Each runs the same triage function the MCP
// tool of the same purpose calls, so CLI output is exactly what an agent sees.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, clientset kubernetes.Interface, args []string, out io.Writer) error
}

var commands = []command{
	{"status", "replica health and rollout state (get_workload_status)", runStatus},
	{"crashloop", "container exit state and previous logs (explain_crashloop)", runCrashloop},
	{"events", "recent, noise-filtered pod events (get_pod_events)", runEvents},
	{"diff", "changes between deployment revisions (diff_deployment_revisions)", runDiff},
}

func findCommand(name string) (command, bool) {
	for _, candidate := range commands {
		if candidate.name == name {
			return candidate, true
		}
	}
	return command{}, false
}

// runCLI executes a subcommand and writes its result as indented JSON.
func runCLI(cmd command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	kubeconfigPath := flags.String("kubeconfig", "", "path to kubeconfig")
	contextName := flags.String("context", "", "kubeconfig context to use")
	// the cluster flags are shared by every subcommand, so they are split out
	// and parsed here; everything else is passed to the subcommand.
	clusterArgs, rest := splitClusterFlags(args)
	if err := flags.Parse(clusterArgs); err != nil {
		return 2
	}

	client, err := kube.New(kube.Options{KubeconfigPath: *kubeconfigPath, Context: *contextName})
	if err != nil {
		writeLine(stderr, fmt.Sprintf("cannot connect to cluster: %v", err))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	if err := cmd.run(ctx, client.Clientset, rest, stdout); err != nil {
		writeLine(stderr, fmt.Sprintf("%s: %v", cmd.name, err))
		return 1
	}
	return 0
}

// splitClusterFlags separates --kubeconfig and --context (which every
// subcommand shares) from the subcommand's own arguments.
func splitClusterFlags(args []string) (cluster, rest []string) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--kubeconfig", "-kubeconfig", "--context", "-context":
			cluster = append(cluster, arg)
			if index+1 < len(args) {
				cluster = append(cluster, args[index+1])
				index++
			}
			continue
		}
		if isClusterFlagWithValue(arg) {
			cluster = append(cluster, arg)
			continue
		}
		rest = append(rest, arg)
	}
	return cluster, rest
}

func isClusterFlagWithValue(arg string) bool {
	for _, prefix := range []string{"--kubeconfig=", "-kubeconfig=", "--context=", "-context="} {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// positional returns the single positional argument a subcommand requires.
func positional(flags *flag.FlagSet, what string) (string, error) {
	if flags.NArg() != 1 {
		return "", fmt.Errorf("expected exactly one %s argument, got %d", what, flags.NArg())
	}
	return flags.Arg(0), nil
}

func runStatus(ctx context.Context, clientset kubernetes.Interface, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	namespace := flags.String("n", "default", "namespace")
	kind := flags.String("kind", "", "Deployment, StatefulSet or DaemonSet (default: detect)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name, err := positional(flags, "workload name")
	if err != nil {
		return err
	}
	result, err := triage.GetWorkloadStatus(ctx, clientset, triage.WorkloadStatusInput{
		Namespace: *namespace, Name: name, Kind: *kind,
	})
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}

func runCrashloop(ctx context.Context, clientset kubernetes.Interface, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("crashloop", flag.ContinueOnError)
	namespace := flags.String("n", "default", "namespace")
	container := flags.String("container", "", "only this container")
	tailLines := flags.Int("tail", 0, "log lines per container (default 50, max 200)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	pod, err := positional(flags, "pod name")
	if err != nil {
		return err
	}
	result, err := triage.ExplainCrashloop(ctx, clientset, triage.CrashloopInput{
		Namespace: *namespace, Pod: pod, Container: *container, TailLines: *tailLines,
	})
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}

func runEvents(ctx context.Context, clientset kubernetes.Interface, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	namespace := flags.String("n", "default", "namespace")
	selector := flags.String("l", "", "label selector (instead of a pod name)")
	since := flags.Int("since", 0, "window in minutes (default 60)")
	limit := flags.Int("limit", 0, "maximum events (default 30, max 100)")
	all := flags.Bool("all", false, "include routine Normal events")
	if err := flags.Parse(args); err != nil {
		return err
	}
	input := triage.PodEventsInput{
		Namespace: *namespace, LabelSelector: *selector,
		SinceMinutes: *since, Limit: *limit, IncludeAll: *all,
	}
	if *selector == "" {
		pod, err := positional(flags, "pod name")
		if err != nil {
			return fmt.Errorf("%w (or pass -l <selector>)", err)
		}
		input.Pod = pod
	}
	result, err := triage.GetPodEvents(ctx, clientset, input)
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}

func runDiff(ctx context.Context, clientset kubernetes.Interface, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("diff", flag.ContinueOnError)
	namespace := flags.String("n", "default", "namespace")
	from := flags.Int64("from", 0, "older revision (default: the one before -to)")
	to := flags.Int64("to", 0, "newer revision (default: latest)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name, err := positional(flags, "deployment name")
	if err != nil {
		return err
	}
	result, err := triage.DiffDeploymentRevisions(ctx, clientset, triage.RevisionDiffInput{
		Namespace: *namespace, Name: name, FromRevision: *from, ToRevision: *to,
	})
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}

// writeLine writes a message for a human to stderr or stdout. A failed write
// to a terminal has no meaningful recovery, so the error is deliberately
// discarded here, in one place, rather than at every call site.
func writeLine(out io.Writer, line string) {
	_, _ = io.WriteString(out, line+"\n")
}

func printUsage(out io.Writer) {
	var usage strings.Builder
	usage.WriteString("usage:\n")
	usage.WriteString("  k8s-triage-mcp [--kubeconfig PATH] [--context NAME]        run the MCP server on stdio\n")
	usage.WriteString("  k8s-triage-mcp <command> [flags] <name>                    run one tool and print JSON\n")
	usage.WriteString("\ncommands:\n")
	for _, cmd := range commands {
		fmt.Fprintf(&usage, "  %-10s %s\n", cmd.name, cmd.summary)
	}
	usage.WriteString("\nevery command accepts --kubeconfig, --context and -n NAMESPACE.")
	writeLine(out, usage.String())
}
