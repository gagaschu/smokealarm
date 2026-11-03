package kube

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// The custom resources are kept as unstructured objects. To save memory, the
// transforms below shrink each object to the few fields the checks read;
// Argo CD Applications in particular carry large resource lists and histories.

// trimCertificate keeps what cert-not-ready and cert-expiry need.
func trimCertificate(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil // e.g. a delete tombstone
	}
	out := newTrimmed(u)
	copyField(u, out, "status", "notAfter")
	copyField(u, out, "status", "conditions")
	return out, nil
}

// trimApplication keeps what argocd-health and argocd-sync need.
func trimApplication(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out := newTrimmed(u)
	copyField(u, out, "status", "health", "status")
	copyField(u, out, "status", "sync", "status")
	return out, nil
}

// newTrimmed creates an object with only the identifying metadata. The
// kubectl last-applied annotation is dropped because it holds a full copy.
func newTrimmed(u *unstructured.Unstructured) *unstructured.Unstructured {
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetNamespace(u.GetNamespace())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	out.SetCreationTimestamp(u.GetCreationTimestamp())

	annotations := u.GetAnnotations()
	delete(annotations, lastAppliedAnnotation)
	if len(annotations) > 0 {
		out.SetAnnotations(annotations)
	}
	return out
}

// copyField copies one (possibly nested) field if it exists.
func copyField(from, to *unstructured.Unstructured, path ...string) {
	value, found, err := unstructured.NestedFieldCopy(from.Object, path...)
	if err != nil || !found {
		return
	}
	// The value comes from a valid object, so setting it cannot fail.
	_ = unstructured.SetNestedField(to.Object, value, path...)
}

// certificateFrom converts a (trimmed) cert-manager Certificate.
func certificateFrom(u *unstructured.Unstructured) checks.Certificate {
	cert := checks.Certificate{Meta: metaOf(u)}

	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		cert.Ready = condition["status"] == "True"
		cert.ReadyReason, _ = condition["reason"].(string)
		cert.ReadyMessage, _ = condition["message"].(string)
	}

	if text, found, _ := unstructured.NestedString(u.Object, "status", "notAfter"); found {
		// An unreadable time stays nil; the check then has nothing to say.
		if t, err := time.Parse(time.RFC3339, text); err == nil {
			cert.NotAfter = &t
		}
	}
	return cert
}

// applicationFrom converts a (trimmed) Argo CD Application.
func applicationFrom(u *unstructured.Unstructured) checks.Application {
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	return checks.Application{Meta: metaOf(u), HealthStatus: health, SyncStatus: sync}
}

func metaOf(u *unstructured.Unstructured) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:              u.GetName(),
		Namespace:         u.GetNamespace(),
		Annotations:       u.GetAnnotations(),
		CreationTimestamp: u.GetCreationTimestamp(),
	}
}
