package kube

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// detectTimeout limits the whole feature detection at startup.
const detectTimeout = 30 * time.Second

// Features tells which optional parts of the cluster are usable. They are
// detected once at startup; CRDs that are installed later, or permissions that
// are granted later, are noticed after a restart.
type Features struct {
	CertManager bool
	ArgoCD      bool
}

// optionalResource describes the custom resource behind one checks.Requires*
// name.
type optionalResource struct {
	gvr   schema.GroupVersionResource
	label string // the product, e.g. "cert-manager"
	name  string // the resource in messages, e.g. certificates.cert-manager.io
	// helmSwitch is the chart value that grants the permission, for the hint
	// in the skip reason.
	helmSwitch string
}

// optionalResourceFor returns the resource that a checks.Requires* name stands
// for. It reports false for a name it does not know.
func optionalResourceFor(requires string) (optionalResource, bool) {
	switch requires {
	case checks.RequiresCertManager:
		return optionalResource{certificateGVR(), "cert-manager", "certificates.cert-manager.io", "rbac.certManager"}, true
	case checks.RequiresArgoCD:
		return optionalResource{applicationGVR(), "Argo CD", "applications.argoproj.io", "rbac.argocd"}, true
	default:
		return optionalResource{}, false
	}
}

// certificateGVR and applicationGVR name the custom resources we read.
func certificateGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
}

func applicationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
}

// DetectFeatures finds out which of the needed optional features can be used.
// "needed" holds the checks.Requires* names of the checks that will run; a
// feature that no check needs is not looked at, so it gets no informer and
// needs no permission.
//
// A needed feature is usable if its CRD is installed and the service account
// may list it. If not, the reason is returned in the second result, keyed by
// the Requires name, and the checks that need it are skipped. Any other error
// (a flaky API, for example) is returned, because it must not silently switch
// checks off.
func DetectFeatures(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface,
	needed []string) (Features, map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	var features Features
	unavailable := map[string]string{}
	for _, requires := range needed {
		resource, known := optionalResourceFor(requires)
		if !known {
			return Features{}, nil, fmt.Errorf("unknown cluster feature %q required by a check", requires)
		}
		reason, err := unusableReason(ctx, client.Discovery(), dynamicClient, resource)
		if err != nil {
			return Features{}, nil, fmt.Errorf("detect %s: %w", resource.name, err)
		}
		if reason != "" {
			unavailable[requires] = reason
			continue
		}
		switch requires {
		case checks.RequiresCertManager:
			features.CertManager = true
		case checks.RequiresArgoCD:
			features.ArgoCD = true
		}
	}
	return features, unavailable, nil
}

// unusableReason returns why a resource cannot be used, or "" if it can.
func unusableReason(ctx context.Context, d discovery.DiscoveryInterface, dynamicClient dynamic.Interface,
	resource optionalResource) (string, error) {
	installed, err := hasResource(d, resource.gvr)
	if err != nil {
		return "", err
	}
	if !installed {
		return fmt.Sprintf("%s CRDs not installed (%s); checked at startup", resource.label, resource.name), nil
	}

	allowed, err := canList(ctx, dynamicClient, resource.gvr)
	if err != nil {
		return "", err
	}
	if !allowed {
		return fmt.Sprintf("missing RBAC permission to list %s: grant it (Helm: %s=true) or disable the checks with SMOKEALARM_DISABLED_CHECKS",
			resource.name, resource.helmSwitch), nil
	}
	return "", nil
}

// canList tries to list one object of the resource. It is the only way to
// learn about a missing permission before the informer starts: an informer
// without permission retries forever and its cache never syncs.
func canList(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource) (bool, error) {
	if dynamicClient == nil {
		return false, fmt.Errorf("a dynamic client is needed to check %s", gvr.Resource)
	}
	_, err := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if apierrors.IsForbidden(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list %s: %w", gvr.Resource, err)
	}
	return true, nil
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
