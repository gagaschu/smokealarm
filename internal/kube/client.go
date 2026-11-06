// Package kube talks to the Kubernetes API. It creates the clients, keeps
// informer caches of the objects we watch, and turns the caches into a
// checks.Snapshot. Checks never see this package; they only get the snapshot.
package kube

import (
	"errors"
	"fmt"

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
}

// NewClients connects to the cluster. Inside a pod it uses the service
// account; anywhere else it uses the usual kubeconfig lookup (KUBECONFIG, then
// ~/.kube/config), which is what local development needs.
func NewClients() (Clients, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	if err != nil {
		return Clients{}, fmt.Errorf("load kubernetes config: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("create dynamic client: %w", err)
	}
	return Clients{Kube: kubeClient, Dynamic: dynamicClient, Config: cfg}, nil
}
