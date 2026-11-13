// Package kube talks to the Kubernetes API. It creates the clients, keeps
// informer caches of the objects we watch, and turns the caches into a
// checks.Snapshot. Checks never see this package; they only get the snapshot.
package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients holds the two API clients we need: the typed one for built-in
// resources and the dynamic one for custom resources (cert-manager, Argo CD),
// whose Go types we do not want as dependencies. Config is what they were
// built from; the kubelet client (NewKubeletClient) uses the same credentials.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Config  *rest.Config
	// Source says where Config came from, for the start log: "in-cluster" or
	// `kubeconfig context "<name>"`. It never holds a credential.
	Source string
}

// NewClients connects to the cluster. See loadRestConfig for which
// configuration it uses. kubeContext is SMOKEALARM_KUBE_CONTEXT.
func NewClients(kubeContext string) (Clients, error) {
	cfg, source, err := loadRestConfig(os.Getenv, rest.InClusterConfig, kubeContext)
	if err != nil {
		return Clients{}, err
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("create dynamic client: %w", err)
	}
	return Clients{Kube: kubeClient, Dynamic: dynamicClient, Config: cfg, Source: source}, nil
}

// loadRestConfig picks the configuration:
//
//  1. If kubeContext is set or KUBECONFIG is not empty, the kubeconfig, even
//     inside a pod. That is how an instance in one cluster watches another;
//     the in-cluster config would silently watch the wrong one.
//  2. Otherwise inside a pod the service account (inCluster).
//  3. Otherwise the usual kubeconfig lookup (~/.kube/config).
//
// getenv and inCluster are injected so tests need no cluster.
func loadRestConfig(getenv func(string) string, inCluster func() (*rest.Config, error), kubeContext string) (*rest.Config, string, error) {
	if kubeContext == "" && getenv("KUBECONFIG") == "" {
		cfg, err := inCluster()
		if err == nil {
			return cfg, "in-cluster", nil
		}
		if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, "", fmt.Errorf("load in-cluster config: %w", err)
		}
	}
	return loadKubeconfig(getenv("KUBECONFIG"), kubeContext)
}

// loadKubeconfig reads the kubeconfig files in path (the KUBECONFIG list; empty
// means ~/.kube/config) and builds the config of kubeContext, or of the
// current context if that is empty.
func loadKubeconfig(path, kubeContext string) (*rest.Config, string, error) {
	// The files are set here instead of read from the environment by
	// client-go, so tests can pass them.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.Precedence = []string{clientcmd.RecommendedHomeFile}
	if path != "" {
		rules.Precedence = filepath.SplitList(path)
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext})

	raw, err := loader.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	name := raw.CurrentContext
	if kubeContext != "" {
		if _, ok := raw.Contexts[kubeContext]; !ok {
			return nil, "", fmt.Errorf("SMOKEALARM_KUBE_CONTEXT: context %q not found in the kubeconfig", kubeContext)
		}
		name = kubeContext
	}

	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, fmt.Sprintf("kubeconfig context %q", name), nil
}
