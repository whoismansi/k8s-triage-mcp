// Command k8s-triage-mcp serves read-only Kubernetes workload diagnostics over
// the Model Context Protocol on stdio.
//
// With a subcommand (status, crashloop, events, diff) it instead runs one tool
// and prints its JSON result, which is useful for scripting and debugging. The
// CLI and the MCP server call the same functions, so their output is identical.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whoismansi/k8s-triage-mcp/internal/kube"
	"github.com/whoismansi/k8s-triage-mcp/internal/mcpserver"
)

func main() {
	if len(os.Args) > 1 {
		if cmd, found := findCommand(os.Args[1]); found {
			os.Exit(runCLI(cmd, os.Args[2:], os.Stdout, os.Stderr))
		}
		if os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
			printUsage(os.Stdout)
			return
		}
	}

	var (
		kubeconfigPath = flag.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG, then ~/.kube/config)")
		contextName    = flag.String("context", "", "kubeconfig context to use (default: current-context)")
		showVersion    = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	if *showVersion {
		fmt.Println(mcpserver.ServerName, mcpserver.Version)
		return
	}

	// stdio carries the MCP protocol, so all logging must go to stderr.
	// a stray Println here would corrupt the stream and break the session.
	logger := log.New(os.Stderr, "", log.LstdFlags)

	client, err := kube.New(kube.Options{
		KubeconfigPath: *kubeconfigPath,
		Context:        *contextName,
	})
	if err != nil {
		logger.Fatalf("cannot connect to cluster: %v", err)
	}
	logger.Printf("connected to context %q at %s (read-only)", client.ContextName, client.Host)

	server := mcpserver.New(client.Clientset)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("server stopped: %v", err)
	}
}
