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
// of a node that is down (one of down, see DownNodes), so the engine can hold them back while the
// node is the real alarm. A consequence is:
//   - a problem of a pod on a down node;
//   - a problem of a Deployment, StatefulSet or DaemonSet whose missing
//     replicas are all explained by the down nodes: no more replicas are
//     unavailable than it has pods (by its selector) on down nodes. Pods on a
//     node that does not answer stay bound to it, also while they are being
//     evicted, so they are still counted then. A workload that misses more,
//     for example a DaemonSet whose rollout fails on the healthy nodes, is
//     not held back.
//
// Nothing else is: a pod that has no node yet (Pending) waits for room, and
// that is worth an alarm of its own.
//
// Like ApplyAnnotations it runs after the checks, so they stay unaware of it.
// The engine passes an empty down when nobody would hear about the node
// (node-not-ready switched off), so nothing is held back then.
func ApplyNodeCauses(snap *Snapshot, findings []Finding, down map[string]bool) []Finding {
	if len(down) == 0 || len(findings) == 0 {
		return findings // the usual case: nothing to look up
	}
	pods := map[string]*corev1.Pod{}
	for _, pod := range snap.Pods {
		pods[pod.Namespace+"/"+pod.Name] = pod
	}
	workloads := workloadsByKey(snap)

	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		switch f.Object.Kind {
		case KindPod:
			if pod := pods[f.Object.Namespace+"/"+f.Object.Name]; pod != nil && down[pod.Spec.NodeName] {
				f.CausedBy = pod.Spec.NodeName
			}
		case KindDeployment, KindStatefulSet, KindDaemonSet:
			if w, found := workloads[objectKey(f.Object)]; found {
				f.CausedBy = w.downNode(snap.Pods, f.Object.Namespace, down)
			}
		}
		out = append(out, f)
	}
	return out
}

// workload is what ApplyNodeCauses needs to know about a Deployment,
// StatefulSet or DaemonSet.
type workload struct {
	selector    labels.Selector
	unavailable int32 // desired minus available replicas
}

// workloadsByKey returns every Deployment, StatefulSet and DaemonSet with a
// usable pod selector, by objectKey. A selector that cannot be parsed is left
// out (the API server does not accept such objects anyway), and so is an empty
// one, which would match every pod in the namespace.
func workloadsByKey(snap *Snapshot) map[string]workload {
	result := map[string]workload{}
	add := func(kind string, meta metav1.ObjectMeta, selector *metav1.LabelSelector, desired, available int32) {
		if selector == nil {
			return
		}
		parsed, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil || parsed.Empty() {
			return
		}
		key := objectKey(ObjectRef{Kind: kind, Namespace: meta.Namespace, Name: meta.Name})
		result[key] = workload{selector: parsed, unavailable: desired - available}
	}
	// The same numbers as WorkloadReplicas.
	for _, o := range snap.Deployments {
		add(KindDeployment, o.ObjectMeta, o.Spec.Selector, desiredReplicas(o.Spec.Replicas), o.Status.AvailableReplicas)
	}
	for _, o := range snap.StatefulSets {
		add(KindStatefulSet, o.ObjectMeta, o.Spec.Selector, desiredReplicas(o.Spec.Replicas), o.Status.AvailableReplicas)
	}
	for _, o := range snap.DaemonSets {
		add(KindDaemonSet, o.ObjectMeta, o.Spec.Selector, o.Status.DesiredNumberScheduled, o.Status.NumberAvailable)
	}
	return result
}

// downNode returns the down node of the workload's first pod on one (in the
// order of the snapshot), if the pods on down nodes explain all its missing
// replicas, and "" otherwise.
func (w workload) downNode(pods []*corev1.Pod, namespace string, down map[string]bool) string {
	first := ""
	var onDownNodes int32
	for _, pod := range pods {
		if pod.Namespace != namespace || !down[pod.Spec.NodeName] || !w.selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		if first == "" {
			first = pod.Spec.NodeName
		}
		onDownNodes++
	}
	if onDownNodes == 0 || w.unavailable > onDownNodes {
		return ""
	}
	return first
}
