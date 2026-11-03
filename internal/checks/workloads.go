package checks

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const checkWorkloadReplicas = "workload-replicas"

// WorkloadReplicas reports Deployments, StatefulSets and DaemonSets that have
// fewer available replicas than desired. Some capacity is missing, which is a
// warning (ReplicasUnavailable). If nothing is available although something
// is wanted, the workload is down: critical (NoReplicasAvailable).
//
// Rolling updates cause short dips; ALERT_AFTER keeps those from alarming.
// Workloads scaled to zero are not a problem and are skipped.
func WorkloadReplicas(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	add := func(kind string, meta metav1.ObjectMeta, desired, available int32) {
		if isIgnored(snap, opts, meta) {
			return
		}
		if f, ok := replicaFinding(kind, meta, desired, available); ok {
			findings = append(findings, f)
		}
	}

	for _, d := range snap.Deployments {
		add("Deployment", d.ObjectMeta, desiredReplicas(d.Spec.Replicas), d.Status.AvailableReplicas)
	}
	for _, s := range snap.StatefulSets {
		add("StatefulSet", s.ObjectMeta, desiredReplicas(s.Spec.Replicas), s.Status.AvailableReplicas)
	}
	for _, d := range snap.DaemonSets {
		// A DaemonSet has no replica count; the controller computes how many
		// nodes should run a pod.
		add("DaemonSet", d.ObjectMeta, d.Status.DesiredNumberScheduled, d.Status.NumberAvailable)
	}
	return findings
}

// replicaFinding compares desired and available replicas of one workload.
func replicaFinding(kind string, meta metav1.ObjectMeta, desired, available int32) (Finding, bool) {
	ref := ObjectRef{Kind: kind, Namespace: meta.Namespace, Name: meta.Name}
	msg := fmt.Sprintf("%d/%d replicas available", available, desired)

	switch {
	case desired == 0 || available >= desired:
		return Finding{}, false
	case available == 0:
		return newFinding(checkWorkloadReplicas, Critical, ref, "NoReplicasAvailable", msg), true
	default:
		return newFinding(checkWorkloadReplicas, Warning, ref, "ReplicasUnavailable", msg), true
	}
}

// desiredReplicas applies the Kubernetes default: no value means one replica.
func desiredReplicas(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}
