package checks

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// CheckNodeNotReady is the name of the NotReady check. It is exported because
// a down node is special beyond the check: it holds back the problems it
// causes (see ApplyNodeCauses), and notifications and the web UI list it first.
const CheckNodeNotReady = "node-not-ready"

const checkNodePressure = "node-pressure"

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
		if !nodeDown(node) {
			continue
		}
		cond := nodeCondition(node, corev1.NodeReady)
		msg := fmt.Sprintf("node is not ready for %s (status %s)",
			snap.Now.Sub(cond.LastTransitionTime.Time).Round(time.Second), cond.Status)
		if cond.Message != "" {
			msg += ": " + cond.Message
		}
		findings = append(findings, newFinding(CheckNodeNotReady, Critical, nodeRef(node), "NotReady", msg))
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
			msg := fmt.Sprintf("node reports %s since %s", pressure, opts.formatTime(cond.LastTransitionTime.Time))
			if cond.Message != "" {
				msg += ": " + cond.Message
			}
			findings = append(findings, newFinding(checkNodePressure, Warning, nodeRef(node), string(pressure), msg))
		}
	}
	return findings
}

// nodeDown says whether the node is not ready: its Ready condition is False or
// Unknown. A node without a Ready condition has just registered and is not
// counted as down.
func nodeDown(node *corev1.Node) bool {
	cond := nodeCondition(node, corev1.NodeReady)
	return cond != nil && cond.Status != corev1.ConditionTrue
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
