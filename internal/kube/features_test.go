package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gagaschu/smokealarm/internal/checks"
)

func apiResources(groupVersion string, names ...string) *metav1.APIResourceList {
	list := &metav1.APIResourceList{GroupVersion: groupVersion}
	for _, name := range names {
		list.APIResources = append(list.APIResources, metav1.APIResource{Name: name})
	}
	return list
}

func TestDetectFeatures(t *testing.T) {
	certManager := apiResources("cert-manager.io/v1", "certificates", "issuers")
	argo := apiResources("argoproj.io/v1alpha1", "applications", "appprojects")
	argoWithoutApps := apiResources("argoproj.io/v1alpha1", "appprojects")

	tests := []struct {
		name      string
		resources []*metav1.APIResourceList
		want      Features
	}{
		{"nothing installed", nil, Features{}},
		{"cert-manager only", []*metav1.APIResourceList{certManager}, Features{CertManager: true}},
		{"argo only", []*metav1.APIResourceList{argo}, Features{ArgoCD: true}},
		{"both", []*metav1.APIResourceList{certManager, argo}, Features{CertManager: true, ArgoCD: true}},
		{"group exists without the resource", []*metav1.APIResourceList{argoWithoutApps}, Features{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			client.Resources = tt.resources

			got, err := DetectFeatures(client)
			if err != nil || got != tt.want {
				t.Errorf("DetectFeatures = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// failingClient is a client whose discovery fails with an error that is not
// "not found", like a connection problem.
type failingClient struct{ kubernetes.Interface }

func (c failingClient) Discovery() discovery.DiscoveryInterfaces {
	return failingDiscovery{c.Interface.Discovery()}
}

type failingDiscovery struct{ discovery.DiscoveryInterfaces }

func (failingDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return nil, errors.New("connection refused")
}

func TestDetectFeaturesFailsOnOtherErrors(t *testing.T) {
	_, err := DetectFeatures(failingClient{fake.NewClientset()})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("got %v, want the discovery error to be returned (not treated as \"not installed\")", err)
	}
}

func TestFeaturesHas(t *testing.T) {
	f := Features{CertManager: true}
	for requires, want := range map[string]bool{
		"": true, checks.RequiresCertManager: true, checks.RequiresArgoCD: false, "unknown": false,
	} {
		if got := f.Has(requires); got != want {
			t.Errorf("Has(%q) = %v, want %v", requires, got, want)
		}
	}
}

func certificateObject(name string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name": name, "namespace": "web", "managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{"dnsNames": []any{"example.com"}, "secretName": "site-tls"},
		"status": map[string]any{
			"notAfter": "2026-12-01T10:00:00Z",
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "Failed", "message": "issuer not found",
			}},
			"revision": int64(3),
		},
	}}
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	return u
}

func applicationObject(name, health, sync string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "argocd"},
		"spec":       map[string]any{"source": map[string]any{"repoURL": "https://example.com/repo.git"}},
		"status": map[string]any{
			"health":         map[string]any{"status": health},
			"sync":           map[string]any{"status": sync},
			"resources":      []any{map[string]any{"kind": "Deployment", "name": "big"}},
			"history":        []any{map[string]any{"id": int64(1)}},
			"operationState": map[string]any{"phase": "Succeeded"},
		},
	}}
}

func TestTrimCertificateKeepsOnlyWhatChecksNeed(t *testing.T) {
	in := certificateObject("site", map[string]string{lastAppliedAnnotation: "{big}", "keep": "me"})

	out, err := trimCertificate(in)
	if err != nil {
		t.Fatalf("trimCertificate: %v", err)
	}
	u := out.(*unstructured.Unstructured)

	if u.GetName() != "site" || u.GetNamespace() != "web" || u.GetKind() != "Certificate" {
		t.Errorf("identity lost: %v", u.Object)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec"); found {
		t.Error("spec must be dropped")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields"); found {
		t.Error("managedFields must be dropped")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "revision"); found {
		t.Error("unused status fields must be dropped")
	}
	if u.GetAnnotations()[lastAppliedAnnotation] != "" || u.GetAnnotations()["keep"] != "me" {
		t.Errorf("annotations = %v, want only keep", u.GetAnnotations())
	}

	cert := certificateFrom(u)
	if cert.Ready || cert.ReadyReason != "Failed" || cert.ReadyMessage != "issuer not found" {
		t.Errorf("condition not converted: %+v", cert)
	}
	if cert.NotAfter == nil || !cert.NotAfter.Equal(time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("NotAfter = %v", cert.NotAfter)
	}
	// The input object must stay untouched; informers share it.
	if _, found, _ := unstructured.NestedFieldNoCopy(in.Object, "spec"); !found {
		t.Error("trimCertificate modified its input")
	}
}

func TestCertificateFromHandlesOddObjects(t *testing.T) {
	tests := []struct {
		name      string
		object    map[string]any
		wantReady bool
		wantTime  bool
	}{
		{"no status at all", map[string]any{"metadata": map[string]any{"name": "c"}}, false, false},
		{"ready", map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			"notAfter":   "2026-12-01T10:00:00Z",
		}}, true, true},
		{"unreadable time", map[string]any{"status": map[string]any{"notAfter": "next tuesday"}}, false, false},
		{"other condition types only", map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Issuing", "status": "True"}},
		}}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert := certificateFrom(&unstructured.Unstructured{Object: tt.object})
			if cert.Ready != tt.wantReady || (cert.NotAfter != nil) != tt.wantTime {
				t.Errorf("got %+v, want ready %v, time set %v", cert, tt.wantReady, tt.wantTime)
			}
		})
	}
}

func TestTrimApplicationKeepsOnlyWhatChecksNeed(t *testing.T) {
	out, err := trimApplication(applicationObject("media", "Degraded", "OutOfSync"))
	if err != nil {
		t.Fatalf("trimApplication: %v", err)
	}
	u := out.(*unstructured.Unstructured)
	for _, path := range [][]string{{"spec"}, {"status", "resources"}, {"status", "history"}, {"status", "operationState"}} {
		if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, path...); found {
			t.Errorf("field %v must be dropped", path)
		}
	}

	app := applicationFrom(u)
	if app.HealthStatus != "Degraded" || app.SyncStatus != "OutOfSync" || app.Meta.Name != "media" || app.Meta.Namespace != "argocd" {
		t.Errorf("application not converted: %+v", app)
	}
}

func TestTrimKeepsTombstones(t *testing.T) {
	for _, trim := range []func(any) (any, error){trimCertificate, trimApplication} {
		out, err := trim("not an object")
		if err != nil || out != "not an object" {
			t.Errorf("got %v, %v; want the value unchanged", out, err)
		}
	}
}

func TestBuildSnapshotWithCustomResources(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			certificateGVR(): "CertificateList",
			applicationGVR(): "ApplicationList",
		},
		certificateObject("site", nil),
		applicationObject("media", "Healthy", "OutOfSync"),
	)
	cluster, err := New(fake.NewClientset(), dyn, Options{Features: Features{CertManager: true, ArgoCD: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	snap := cluster.BuildSnapshot(context.Background(), time.Now())
	if len(snap.Certificates) != 1 || snap.Certificates[0].Meta.Name != "site" || snap.Certificates[0].ReadyReason != "Failed" {
		t.Errorf("Certificates = %+v", snap.Certificates)
	}
	if len(snap.Applications) != 1 || snap.Applications[0].SyncStatus != "OutOfSync" {
		t.Errorf("Applications = %+v", snap.Applications)
	}
}

func TestNewWithoutFeaturesHasNoCustomResources(t *testing.T) {
	cluster, err := New(fake.NewClientset(), nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := cluster.BuildSnapshot(context.Background(), time.Now())
	if snap.Certificates != nil || snap.Applications != nil {
		t.Errorf("got %+v / %+v, want none without the CRDs", snap.Certificates, snap.Applications)
	}
}

func TestNewNeedsDynamicClientForFeatures(t *testing.T) {
	_, err := New(fake.NewClientset(), nil, Options{Features: Features{ArgoCD: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Error("expected an error: Argo CD informers need the dynamic client")
	}
}
