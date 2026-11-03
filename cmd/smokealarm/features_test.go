package main

import (
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/checks"
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
