package checks

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// DownNodes returns the names of the nodes that are down (see nodeDown). The
// ignore annotation of a node does not count here: an ignored test node that
// fails still causes problems, and those are held back like any others.
func DownNodes(snap *Snapshot) map[string]bool {
	down := map[string]bool{}
	for _, node := range snap.Nodes {
		if nodeDown(node) {
			down[node.Name] = true
		}
	}
	return down
}

// ApplyNodeCauses sets Finding.CausedBy for the problems that are consequences
// of a node that is down, so the engine can hold them back while the node is
// the real alarm. A consequence is a problem of a pod on a down node, or of a
// Deployment, StatefulSet or DaemonSet with at least one of its pods on a down
// node. Nothing else is: a pod that has no node yet (Pending) waits for room,
// and that is worth an alarm of its own.
//
// Like ApplyAnnotations it runs after the checks, so they stay unaware of it.
func ApplyNodeCauses(snap *Snapshot, findings []Finding) []Finding {
	down := DownNodes(snap)
	if len(down) == 0 || len(findings) == 0 {
		return findings // the usual case: nothing to look up
	}
	pods := map[string]*corev1.Pod{}
	for _, pod := range snap.Pods {
		pods[pod.Namespace+"/"+pod.Name] = pod
	}
	selectors := workloadSelectors(snap)

	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		switch f.Object.Kind {
		case KindPod:
			if pod := pods[f.Object.Namespace+"/"+f.Object.Name]; pod != nil && down[pod.Spec.NodeName] {
				f.CausedBy = pod.Spec.NodeName
			}
		case KindDeployment, KindStatefulSet, KindDaemonSet:
			if selector, found := selectors[objectKey(f.Object)]; found {
				f.CausedBy = downNodeOfPods(snap.Pods, f.Object.Namespace, selector, down)
			}
		}
		out = append(out, f)
	}
	return out
}

// workloadSelectors returns the pod selector of every Deployment, StatefulSet
// and DaemonSet, by objectKey. A selector that cannot be parsed is left out;
// the API server does not accept such objects anyway.
func workloadSelectors(snap *Snapshot) map[string]labels.Selector {
	selectors := map[string]labels.Selector{}
	add := func(kind string, meta metav1.ObjectMeta, selector *metav1.LabelSelector) {
		if selector == nil {
			return
		}
		parsed, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil || parsed.Empty() {
			return // an empty selector would match every pod in the namespace
		}
		selectors[objectKey(ObjectRef{Kind: kind, Namespace: meta.Namespace, Name: meta.Name})] = parsed
	}
	for _, o := range snap.Deployments {
		add(KindDeployment, o.ObjectMeta, o.Spec.Selector)
	}
	for _, o := range snap.StatefulSets {
		add(KindStatefulSet, o.ObjectMeta, o.Spec.Selector)
	}
	for _, o := range snap.DaemonSets {
		add(KindDaemonSet, o.ObjectMeta, o.Spec.Selector)
	}
	return selectors
}

// downNodeOfPods returns the down node of the first matching pod (in the order
// of the snapshot) that runs on one, or "" if there is none.
func downNodeOfPods(pods []*corev1.Pod, namespace string, selector labels.Selector, down map[string]bool) string {
	for _, pod := range pods {
		if pod.Namespace == namespace && down[pod.Spec.NodeName] && selector.Matches(labels.Set(pod.Labels)) {
			return pod.Spec.NodeName
		}
	}
	return ""
}
