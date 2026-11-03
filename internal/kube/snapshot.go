package kube

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// BuildSnapshot copies the current cache contents into a snapshot. The
// objects are shared with the cache, so checks must treat them as read-only.
//
// The loops are written out per type on purpose: without generics, a
// shared helper would need reflection, which is harder to read.
func (c *Cluster) BuildSnapshot(now time.Time) *checks.Snapshot {
	snap := &checks.Snapshot{Now: now}

	for _, obj := range c.namespaces.GetStore().List() {
		if ns, ok := obj.(*corev1.Namespace); ok {
			snap.Namespaces = append(snap.Namespaces, ns)
		}
	}
	for _, obj := range c.pods.GetStore().List() {
		if pod, ok := obj.(*corev1.Pod); ok {
			snap.Pods = append(snap.Pods, pod)
		}
	}
	for _, obj := range c.events.GetStore().List() {
		if event, ok := obj.(*corev1.Event); ok {
			snap.Events = append(snap.Events, event)
		}
	}
	for _, obj := range c.deployments.GetStore().List() {
		if d, ok := obj.(*appsv1.Deployment); ok {
			snap.Deployments = append(snap.Deployments, d)
		}
	}
	for _, obj := range c.statefulSets.GetStore().List() {
		if s, ok := obj.(*appsv1.StatefulSet); ok {
			snap.StatefulSets = append(snap.StatefulSets, s)
		}
	}
	for _, obj := range c.daemonSets.GetStore().List() {
		if d, ok := obj.(*appsv1.DaemonSet); ok {
			snap.DaemonSets = append(snap.DaemonSets, d)
		}
	}
	for _, obj := range c.nodes.GetStore().List() {
		if node, ok := obj.(*corev1.Node); ok {
			snap.Nodes = append(snap.Nodes, node)
		}
	}
	for _, obj := range c.jobs.GetStore().List() {
		if job, ok := obj.(*batchv1.Job); ok {
			snap.Jobs = append(snap.Jobs, job)
		}
	}
	for _, obj := range c.cronJobs.GetStore().List() {
		if cj, ok := obj.(*batchv1.CronJob); ok {
			snap.CronJobs = append(snap.CronJobs, cj)
		}
	}
	for _, obj := range c.pvcs.GetStore().List() {
		if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			snap.PVCs = append(snap.PVCs, pvc)
		}
	}
	for _, obj := range c.storageClasses.GetStore().List() {
		if class, ok := obj.(*storagev1.StorageClass); ok {
			snap.StorageClasses = append(snap.StorageClasses, class)
		}
	}
	c.addCustomResourcesToSnapshot(snap)
	return snap
}

// addCustomResourcesToSnapshot converts the cert-manager and Argo CD objects,
// if those informers exist.
func (c *Cluster) addCustomResourcesToSnapshot(snap *checks.Snapshot) {
	if c.certificates != nil {
		for _, obj := range c.certificates.GetStore().List() {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				snap.Certificates = append(snap.Certificates, certificateFrom(u))
			}
		}
	}
	if c.applications != nil {
		for _, obj := range c.applications.GetStore().List() {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				snap.Applications = append(snap.Applications, applicationFrom(u))
			}
		}
	}
}
