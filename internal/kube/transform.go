package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// lastAppliedAnnotation can be very large (it holds a full copy of the
// object) and no check needs it.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// trimObject runs on every object before it enters an informer cache. It
// drops fields that cost memory but that no check reads, which matters on a
// small machine like a Raspberry Pi.
func trimObject(obj any) (any, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		// Not a regular object (e.g. a delete tombstone): keep it as is.
		return obj, nil
	}
	accessor.SetManagedFields(nil)
	if annotations := accessor.GetAnnotations(); annotations[lastAppliedAnnotation] != "" {
		delete(annotations, lastAppliedAnnotation)
		accessor.SetAnnotations(annotations)
	}

	if pod, ok := obj.(*corev1.Pod); ok {
		trimPod(pod)
	}
	return obj, nil
}

// trimPod removes the parts of a pod spec that are large and unused:
// volumes, environment and mounts. Secrets can show up in env values, so not
// keeping them is also a small safety win.
func trimPod(pod *corev1.Pod) {
	pod.Spec.Volumes = nil
	for i := range pod.Spec.Containers {
		trimContainer(&pod.Spec.Containers[i])
	}
	for i := range pod.Spec.InitContainers {
		trimContainer(&pod.Spec.InitContainers[i])
	}
}

func trimContainer(c *corev1.Container) {
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
}
