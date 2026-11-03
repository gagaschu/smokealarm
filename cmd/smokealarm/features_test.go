package main

import (
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/kube"
)

func TestSplitByFeatures(t *testing.T) {
	enabled := checks.All()

	tests := []struct {
		name            string
		features        kube.Features
		wantUnavailable []string
	}{
		{"nothing installed", kube.Features{}, []string{"cert-not-ready", "cert-expiry", "argocd-health", "argocd-sync"}},
		{"cert-manager only", kube.Features{CertManager: true}, []string{"argocd-health", "argocd-sync"}},
		{"argo only", kube.Features{ArgoCD: true}, []string{"cert-not-ready", "cert-expiry"}},
		{"both", kube.Features{CertManager: true, ArgoCD: true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, unavailable := splitByFeatures(enabled, tt.features)

			var names []string
			for _, c := range unavailable {
				names = append(names, c.Name)
				if !strings.Contains(c.Reason, "not installed") {
					t.Errorf("%s: reason %q does not say what is missing", c.Name, c.Reason)
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

func TestSplitByFeaturesKeepsDisabledChecksOut(t *testing.T) {
	enabled, err := checks.Select([]string{"pvc-usage", "cert-expiry"})
	if err != nil {
		t.Fatal(err)
	}
	active, unavailable := splitByFeatures(enabled, kube.Features{CertManager: true})
	if hasCheck(active, checks.PVCUsageCheck) || hasCheck(active, "cert-expiry") {
		t.Error("checks disabled by configuration must not come back")
	}
	if len(unavailable) != 2 { // argocd-health and argocd-sync
		t.Errorf("unavailable = %+v, want only the Argo CD checks", unavailable)
	}
}

func TestFeaturesInUse(t *testing.T) {
	all := kube.Features{CertManager: true, ArgoCD: true}

	tests := []struct {
		name     string
		disabled []string
		detected kube.Features
		want     kube.Features
	}{
		{"all checks active", nil, all, all},
		{"cert-manager checks disabled", []string{"cert-not-ready", "cert-expiry"}, all, kube.Features{ArgoCD: true}},
		{"only one of the two cert-manager checks disabled", []string{"cert-expiry"}, all, all},
		{"all Argo CD checks disabled", []string{"argocd-health", "argocd-sync"}, all, kube.Features{CertManager: true}},
		{"everything disabled", []string{"cert-not-ready", "cert-expiry", "argocd-health", "argocd-sync"}, all, kube.Features{}},
		{"CRDs not installed", nil, kube.Features{}, kube.Features{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, err := checks.Select(tt.disabled)
			if err != nil {
				t.Fatal(err)
			}
			active, _ := splitByFeatures(enabled, tt.detected)

			if got := featuresInUse(active, tt.detected); got != tt.want {
				t.Errorf("featuresInUse = %+v, want %+v", got, tt.want)
			}
		})
	}
}
