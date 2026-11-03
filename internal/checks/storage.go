package checks

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

const checkPVCPending = "pvc-pending"

// defaultClassAnnotation marks the StorageClass used by PVCs that name none.
const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// PVCPending reports PersistentVolumeClaims that stay Pending longer than the
// configured time: no volume could be provisioned or bound, so every pod
// that needs the claim cannot start. Severity is warning.
//
// Claims of a StorageClass with volumeBindingMode WaitForFirstConsumer are
// skipped. They stay Pending on purpose until a pod uses them (k3s local-path
// works like this), so an unused claim would alarm forever. A pod that is
// stuck waiting for such a claim is already reported by PodPending.
func PVCPending(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pvc := range snap.PVCs {
		if pvc.Status.Phase != corev1.ClaimPending || isIgnored(snap, opts, pvc.ObjectMeta) {
			continue
		}
		age := snap.Now.Sub(pvc.CreationTimestamp.Time)
		if age <= opts.PVCPendingAfter || waitsForFirstConsumer(snap, pvc) {
			continue
		}
		ref := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name}
		msg := fmt.Sprintf("pending for %s: %s", age.Round(time.Second), pvcDetail(snap, ref))
		findings = append(findings, newFinding(checkPVCPending, Warning, ref, "PVCPending", msg))
	}
	return findings
}

// pvcDetail explains why a claim is pending, taken from the newest warning
// event (for example ProvisioningFailed).
func pvcDetail(snap *Snapshot, ref ObjectRef) string {
	if e := latestWarning(snap, ref); e != nil {
		return fmt.Sprintf("%s: %s", e.Reason, e.Message)
	}
	return "no details available"
}

// waitsForFirstConsumer reports whether the claim's StorageClass delays
// binding until a pod uses the claim.
func waitsForFirstConsumer(snap *Snapshot, pvc *corev1.PersistentVolumeClaim) bool {
	class := storageClassOf(snap, pvc)
	return class != nil && class.VolumeBindingMode != nil &&
		*class.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
}

// storageClassOf finds the StorageClass of a claim: the named one, or the
// default class if the claim names none. An empty name means "no class" on
// purpose (static binding), so the result is nil.
func storageClassOf(snap *Snapshot, pvc *corev1.PersistentVolumeClaim) *storagev1.StorageClass {
	name := pvc.Spec.StorageClassName
	if name != nil && *name == "" {
		return nil
	}
	for _, class := range snap.StorageClasses {
		if name != nil && class.Name == *name {
			return class
		}
		if name == nil && class.Annotations[defaultClassAnnotation] == "true" {
			return class
		}
	}
	return nil
}
