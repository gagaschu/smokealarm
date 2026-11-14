// Package kube talks to the Kubernetes API. It creates the clients, keeps
// informer caches of the objects we watch, and turns the caches into a
// checks.Snapshot. Checks never see this package; they only get the snapshot.
package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	cfg, source, err := loadRestConfig(os.Getenv, rest.InClusterConfig, clientcmd.RecommendedHomeFile, kubeContext)
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
//  3. Otherwise the kubeconfig in homeFile (~/.kube/config).
//
// getenv, inCluster and homeFile are injected so tests need no cluster and do
// not read the kubeconfig of the machine they run on.
func loadRestConfig(getenv func(string) string, inCluster func() (*rest.Config, error), homeFile, kubeContext string) (*rest.Config, string, error) {
	files := filepath.SplitList(getenv("KUBECONFIG"))
	if kubeContext == "" && len(files) == 0 {
		cfg, err := inCluster()
		if err == nil {
			return cfg, "in-cluster", nil
		}
		if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, "", fmt.Errorf("load in-cluster config: %w", err)
		}
	}
	if len(files) == 0 {
		files = []string{homeFile}
	}
	return loadKubeconfig(files, kubeContext)
}

// loadKubeconfig reads the kubeconfig files and builds the config of
// kubeContext, or of the current context if that is empty.
//
// It never falls back to the in-cluster config: client-go's deferred loader
// does that for a missing or empty kubeconfig, and smokealarm would then watch
// the cluster it runs in although it was told to watch another.
func loadKubeconfig(files []string, kubeContext string) (*rest.Config, string, error) {
	// client-go skips files that do not exist, so a wrong path would only show
	// as an empty configuration.
	found := false
	for _, file := range files {
		if _, err := os.Stat(file); err == nil {
			found = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	list := strings.Join(files, string(filepath.ListSeparator))
	if !found {
		return nil, "", fmt.Errorf("load kubeconfig: %s does not exist (set by KUBECONFIG, or ~/.kube/config without it)", list)
	}

	rules := &clientcmd.ClientConfigLoadingRules{Precedence: files}
	raw, err := rules.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig %s: %w", list, err)
	}
	name := raw.CurrentContext
	if kubeContext != "" {
		if _, ok := raw.Contexts[kubeContext]; !ok {
			return nil, "", fmt.Errorf("SMOKEALARM_KUBE_CONTEXT: context %q not found in the kubeconfig %s", kubeContext, list)
		}
		name = kubeContext
	}
	if name == "" {
		return nil, "", fmt.Errorf("load kubeconfig %s: it has no current-context; set one or name the context with SMOKEALARM_KUBE_CONTEXT", list)
	}

	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig %s: %w", list, err)
	}
	return cfg, fmt.Sprintf("kubeconfig context %q", name), nil
}
