// Package kube talks to the Kubernetes API. It creates the client, keeps
// informer caches of the objects we watch, and turns the caches into a
// checks.Snapshot. Checks never see this package; they only get the snapshot.
package kube

import (
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient connects to the cluster. Inside a pod it uses the service account;
// anywhere else it uses the usual kubeconfig lookup (KUBECONFIG, then
// ~/.kube/config), which is what local development needs.
func NewClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("load kubernetes config: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return client, nil
}
