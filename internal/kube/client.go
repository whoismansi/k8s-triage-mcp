// Package kube handles connecting to a cluster using the same kubeconfig
// resolution rules kubectl uses, so the server talks to whatever cluster the
// operator is already pointed at.
package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Options controls how the client locates and selects a cluster.
type Options struct {
	// KubeconfigPath overrides the default kubeconfig lookup when non-empty.
	KubeconfigPath string
	// Context selects a named context from the kubeconfig. Empty means use the
	// kubeconfig's current-context.
	Context string
}

// Client bundles a clientset with the cluster identity it resolved to, so the
// server can report which cluster it is reading without a second lookup.
type Client struct {
	Clientset kubernetes.Interface
	// ContextName is the kubeconfig context in use.
	ContextName string
	// Host is the API server URL, useful for confirming which cluster is live.
	Host string
}

// New builds a Kubernetes client from kubeconfig.
//
// Resolution follows the standard clientcmd loading rules: an explicit path if
// given, otherwise $KUBECONFIG, otherwise ~/.kube/config. If running inside a
// cluster with no kubeconfig present, clientcmd falls back to the service
// account token automatically.
//
// This client is only ever used for read calls, and is intended to run under
// a ClusterRole that grants only get, list and watch, with no access to
// Secrets or ConfigMaps.
func New(opts Options) (*Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.KubeconfigPath != "" {
		loadingRules.ExplicitPath = opts.KubeconfigPath
	}

	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	applyReadOnlyDefaults(restConfig)

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}

	// resolve the effective context name for reporting. a failure here is not
	// fatal: the client works regardless, we just cannot name the context.
	contextName := opts.Context
	if contextName == "" {
		if rawConfig, rawErr := clientConfig.RawConfig(); rawErr == nil {
			contextName = rawConfig.CurrentContext
		}
	}

	return &Client{
		Clientset:   clientset,
		ContextName: contextName,
		Host:        restConfig.Host,
	}, nil
}

// applyReadOnlyDefaults sets a user agent that identifies this server in API
// server audit logs. Anyone auditing a cluster should be able to see at a
// glance that these calls came from the triage server.
func applyReadOnlyDefaults(config *rest.Config) {
	config.UserAgent = "k8s-triage-mcp (read-only)"
}
