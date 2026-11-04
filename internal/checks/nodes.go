package checks

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	checkNodeNotReady = "node-not-ready"
	checkNodePressure = "node-pressure"
)

// NodeNotReady reports nodes whose Ready condition is False or Unknown
// (Unknown means the node stopped reporting). Pods on the node are not
// running or will be evicted, so the severity is critical.
//
// Nodes without any Ready condition are skipped: they have just registered
// and the kubelet has not reported yet.
func NodeNotReady(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, node := range snap.Nodes {
		if isIgnored(snap, opts, node.ObjectMeta) {
			continue
		}
		cond := nodeCondition(node, corev1.NodeReady)
		if cond == nil || cond.Status == corev1.ConditionTrue {
			continue
		}
		msg := fmt.Sprintf("node is not ready for %s (status %s)",
			snap.Now.Sub(cond.LastTransitionTime.Time).Round(time.Second), cond.Status)
		if cond.Message != "" {
			msg += ": " + cond.Message
		}
		findings = append(findings, newFinding(checkNodeNotReady, Critical, nodeRef(node), "NotReady", msg))
	}
	return findings
}

// NodePressure reports nodes under MemoryPressure, DiskPressure or
// PIDPressure. The kubelet starts evicting pods under pressure, so this is
// an early warning. The reason is the condition type, so each kind of
// pressure is its own finding.
func NodePressure(snap *Snapshot, opts Options) []Finding {
	pressures := []corev1.NodeConditionType{
		corev1.NodeMemoryPressure,
		corev1.NodeDiskPressure,
		corev1.NodePIDPressure,
	}

	var findings []Finding
	for _, node := range snap.Nodes {
		if isIgnored(snap, opts, node.ObjectMeta) {
			continue
		}
		for _, pressure := range pressures {
			cond := nodeCondition(node, pressure)
			if cond == nil || cond.Status != corev1.ConditionTrue {
				continue
			}
			msg := fmt.Sprintf("node reports %s since %s", pressure, cond.LastTransitionTime.Format(time.RFC3339))
			if cond.Message != "" {
				msg += ": " + cond.Message
			}
			findings = append(findings, newFinding(checkNodePressure, Warning, nodeRef(node), string(pressure), msg))
		}
	}
	return findings
}

// nodeCondition returns the condition of the given type, or nil.
func nodeCondition(node *corev1.Node, condType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == condType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func nodeRef(node *corev1.Node) ObjectRef {
	return ObjectRef{Kind: KindNode, Name: node.Name}
}
