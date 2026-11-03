package checks

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

const checkPVCUsage = "pvc-usage"

// PVCUsageCheck is the name of the pvc-usage check. The kube package needs it
// to say that the check could not run when the kubelet stats are unavailable.
const PVCUsageCheck = checkPVCUsage

// Usage thresholds from the specification, in percent.
const (
	pvcWarnPercent     = 85
	pvcCriticalPercent = 95
)

// PVCUsage reports volumes that are almost full, based on the kubelet stats:
// more than 85 % used is a warning (VolumeAlmostFull), more than 95 % is
// critical (VolumeFull). A full volume makes writes fail, often without a
// clear error in the application. Exactly 85 % or 95 % do not alarm yet.
func PVCUsage(snap *Snapshot, opts Options) []Finding {
	keys := make([]string, 0, len(snap.VolumeUsage))
	for key := range snap.VolumeUsage {
		keys = append(keys, key)
	}
	sort.Strings(keys) // stable output order

	var findings []Finding
	for _, key := range keys {
		usage := snap.VolumeUsage[key]
		namespace, name, found := strings.Cut(key, "/")
		if !found || usage.CapacityBytes <= 0 {
			continue
		}
		if isIgnored(snap, opts, pvcMeta(snap, namespace, name)) {
			continue
		}

		// Integer math: no rounding surprises at the thresholds.
		var severity Severity
		var reason string
		switch {
		case usage.UsedBytes*100 > usage.CapacityBytes*pvcCriticalPercent:
			severity, reason = Critical, "VolumeFull"
		case usage.UsedBytes*100 > usage.CapacityBytes*pvcWarnPercent:
			severity, reason = Warning, "VolumeAlmostFull"
		default:
			continue
		}
		// One decimal, so 85.2 % does not read as "85 %" next to a warning
		// that starts above 85.
		msg := fmt.Sprintf("%.1f%% full (%s of %s used)",
			float64(usage.UsedBytes)*100/float64(usage.CapacityBytes), formatBytes(usage.UsedBytes), formatBytes(usage.CapacityBytes))
		ref := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: namespace, Name: name}
		findings = append(findings, newEscalatingFinding(checkPVCUsage, severity, ref, "usage", reason, msg))
	}
	return findings
}

// pvcMeta returns the metadata of the claim if it is in the snapshot, so its
// ignore annotation counts. Otherwise only the namespace rules apply.
func pvcMeta(snap *Snapshot, namespace, name string) metav1.ObjectMeta {
	for _, pvc := range snap.PVCs {
		if pvc.Namespace == namespace && pvc.Name == name {
			return pvc.ObjectMeta
		}
	}
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

// formatBytes writes a size in the largest fitting binary unit, e.g. "9.1 GiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), ""
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		suffix = s
		if value < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
