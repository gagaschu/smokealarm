package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/kube"
)

const (
	certManagerReason = "cert-manager CRDs not installed (certificates.cert-manager.io); checked at startup"
	argoReason        = "missing RBAC permission to list applications.argoproj.io"
)

func TestSplitByFeatures(t *testing.T) {
	enabled := checks.All()

	tests := []struct {
		name            string
		notUsable       map[string]string
		wantUnavailable []string
	}{
		{"everything usable", nil, nil},
		{"cert-manager missing", map[string]string{checks.RequiresCertManager: certManagerReason},
			[]string{"cert-not-ready", "cert-expiry"}},
		{"argo missing", map[string]string{checks.RequiresArgoCD: argoReason},
			[]string{"argocd-health", "argocd-sync"}},
		{"both missing", map[string]string{checks.RequiresCertManager: certManagerReason, checks.RequiresArgoCD: argoReason},
			[]string{"cert-not-ready", "cert-expiry", "argocd-health", "argocd-sync"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, unavailable := splitByFeatures(enabled, tt.notUsable)

			var names []string
			for _, c := range unavailable {
				names = append(names, c.Name)
				// Each skipped check shows the reason of its own feature.
				check := findCheck(t, c.Name)
				if c.Reason != tt.notUsable[check.Requires] {
					t.Errorf("%s: reason %q, want %q", c.Name, c.Reason, tt.notUsable[check.Requires])
				}
			}
			if strings.Join(names, ",") != strings.Join(tt.wantUnavailable, ",") {
				t.Errorf("unavailable = %v, want %v", names, tt.wantUnavailable)
			}
			if len(active)+len(unavailable) != len(enabled) {
				t.Errorf("%d active + %d unavailable != %d enabled", len(active), len(unavailable), len(enabled))
			}
			// Core checks and pvc-usage never need a CRD.
			if !hasCheck(active, "pod-crashloop") || !hasCheck(active, checks.PVCUsageCheck) {
				t.Error("checks without requirements must always stay active")
			}
		})
	}
}

func findCheck(t *testing.T, name string) checks.Check {
	t.Helper()
	for _, c := range checks.All() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %s", name)
	return checks.Check{}
}

func TestSplitByFeaturesKeepsDisabledChecksOut(t *testing.T) {
	enabled, err := checks.Select([]string{"pvc-usage", "cert-expiry"})
	if err != nil {
		t.Fatal(err)
	}
	active, unavailable := splitByFeatures(enabled, map[string]string{checks.RequiresArgoCD: argoReason})
	if hasCheck(active, checks.PVCUsageCheck) || hasCheck(active, "cert-expiry") {
		t.Error("checks disabled by configuration must not come back")
	}
	if len(unavailable) != 2 { // argocd-health and argocd-sync
		t.Errorf("unavailable = %+v, want only the Argo CD checks", unavailable)
	}
}

// requirements decides which optional CRDs are looked at, and therefore which
// ones get an informer. A CRD whose checks are all disabled must not be listed.
func TestRequirements(t *testing.T) {
	tests := []struct {
		name     string
		disabled []string
		want     string
	}{
		{"all checks enabled", nil, "cert-manager,argocd"},
		{"cert-manager checks disabled", []string{"cert-not-ready", "cert-expiry"}, "argocd"},
		{"only one of the two cert-manager checks disabled", []string{"cert-expiry"}, "cert-manager,argocd"},
		{"all Argo CD checks disabled", []string{"argocd-health", "argocd-sync"}, "cert-manager"},
		{"everything optional disabled", []string{"cert-not-ready", "cert-expiry", "argocd-health", "argocd-sync"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, err := checks.Select(tt.disabled)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(requirements(enabled), ","); got != tt.want {
				t.Errorf("requirements = %q, want %q", got, tt.want)
			}
		})
	}
}

// chooseChecks ties the detection to the check list: Argo CD is installed but
// the service account may not list Applications, so its checks are skipped with
// the permission as the reason, and no informer would be started for it.
func TestChooseChecksSkipsChecksWithoutPermission(t *testing.T) {
	client := fake.NewClientset()
	client.Resources = []*metav1.APIResourceList{
		{GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{{Name: "certificates"}}},
		{GroupVersion: "argoproj.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "applications"}}},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}:   "CertificateList",
			{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}: "ApplicationList",
		})
	dyn.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "applications"}, "", errors.New("RBAC: access denied"))
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	active, unavailable, features, err := chooseChecks(context.Background(), logger,
		kube.Clients{Kube: client, Dynamic: dyn}, checks.All())
	if err != nil {
		t.Fatalf("chooseChecks: %v", err)
	}

	if features != (kube.Features{CertManager: true}) {
		t.Errorf("features = %+v, want only cert-manager (Argo CD cannot be read)", features)
	}
	if !hasCheck(active, "cert-expiry") || hasCheck(active, "argocd-health") || hasCheck(active, "argocd-sync") {
		t.Errorf("active checks do not match: cert-expiry must stay, the Argo CD checks must go")
	}
	if len(unavailable) != 2 {
		t.Fatalf("unavailable = %+v, want the two Argo CD checks", unavailable)
	}
	for _, c := range unavailable {
		if !strings.Contains(c.Reason, "missing RBAC permission") || !strings.Contains(c.Reason, "rbac.argocd") {
			t.Errorf("%s: reason %q should name the missing permission and the chart switch", c.Name, c.Reason)
		}
	}
}
