package checks

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AnnotationPrefix is the one place that defines our annotation domain.
const AnnotationPrefix = "smokealarm.gagaschu.github.io"

// IgnoreAnnotation set to "true" on an object or its namespace suppresses
// all findings for it.
const IgnoreAnnotation = AnnotationPrefix + "/ignore"

// isIgnored reports whether findings for the object must be suppressed:
// the namespace is on the ignore list, or the object or its namespace carries
// the ignore annotation. For pods the annotation is read from the pod itself,
// which gets it copied from the pod template of its owner.
func isIgnored(snap *Snapshot, opts Options, meta metav1.ObjectMeta) bool {
	if meta.Annotations[IgnoreAnnotation] == "true" {
		return true
	}
	if meta.Namespace == "" {
		return false
	}
	if slices.Contains(opts.IgnoredNamespaces, meta.Namespace) {
		return true
	}
	for _, ns := range snap.Namespaces {
		if ns.Name == meta.Namespace {
			return ns.Annotations[IgnoreAnnotation] == "true"
		}
	}
	return false
}

// latestWarning returns the newest Warning event for an object, or nil.
// Events add context to findings, e.g. why a pod cannot be scheduled.
func latestWarning(snap *Snapshot, ref ObjectRef) *corev1.Event {
	var newest *corev1.Event
	var newestTime time.Time
	for _, e := range snap.Events {
		involved := e.InvolvedObject
		if e.Type != corev1.EventTypeWarning || involved.Kind != ref.Kind ||
			involved.Namespace != ref.Namespace || involved.Name != ref.Name {
			continue
		}
		if t := eventTime(e); newest == nil || t.After(newestTime) {
			newest, newestTime = e, t
		}
	}
	return newest
}

// eventTime picks the most precise timestamp an event has. Older clusters
// fill LastTimestamp, newer event sources only fill EventTime.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
