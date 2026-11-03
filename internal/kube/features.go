package kube

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Features tells which optional parts the cluster has. They are detected once
// at startup; CRDs that are installed later are noticed after a restart.
type Features struct {
	CertManager bool
	ArgoCD      bool
}

// Has reports whether the feature behind a checks.Requires* name is present.
// An empty name means "nothing required".
func (f Features) Has(requires string) bool {
	switch requires {
	case "":
		return true
	case checks.RequiresCertManager:
		return f.CertManager
	case checks.RequiresArgoCD:
		return f.ArgoCD
	default:
		return false
	}
}

// certificateGVR and applicationGVR name the custom resources we read.
func certificateGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
}

func applicationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
}

// DetectFeatures asks the discovery API which of the optional resources
// exist. "Not found" means "not installed"; any other error is returned,
// because a flaky API must not silently switch checks off.
func DetectFeatures(client kubernetes.Interface) (Features, error) {
	var features Features
	var err error

	if features.CertManager, err = hasResource(client.Discovery(), certificateGVR()); err != nil {
		return Features{}, fmt.Errorf("detect cert-manager: %w", err)
	}
	if features.ArgoCD, err = hasResource(client.Discovery(), applicationGVR()); err != nil {
		return Features{}, fmt.Errorf("detect Argo CD: %w", err)
	}
	return features, nil
}

func hasResource(d discovery.DiscoveryInterface, gvr schema.GroupVersionResource) (bool, error) {
	list, err := d.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list resources of %s: %w", gvr.GroupVersion(), err)
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}
