// Package checks contains the health checks. A check is a plain function:
// it receives a Snapshot of cluster objects and returns Findings. Checks never
// call the Kubernetes API and keep no state, so they can be tested with
// hand-built objects.
package checks

import (
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Severity says how urgent a finding is.
type Severity string

// The two severities. critical means "act now", warning means "look soon".
const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
)

// ObjectRef points at the Kubernetes object a finding is about.
// Namespace is empty for cluster-scoped objects such as nodes.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
}

// Finding is one detected problem.
type Finding struct {
	// Key identifies "the same problem" across runs. It is built from the
	// check, the object and the reason, so it must not contain anything
	// that changes while the problem persists (counters, timestamps).
	Key      string
	Check    string
	Severity Severity
	Object   ObjectRef
	Reason   string // short and stable, e.g. "CrashLoopBackOff"
	Message  string // human readable details, may change between runs
}

// Snapshot is a read-only copy of the cluster objects the checks need.
type Snapshot struct {
	Now            time.Time
	Namespaces     []*corev1.Namespace
	Pods           []*corev1.Pod
	Events         []*corev1.Event // Warning events only
	Deployments    []*appsv1.Deployment
	StatefulSets   []*appsv1.StatefulSet
	DaemonSets     []*appsv1.DaemonSet
	Nodes          []*corev1.Node
	Jobs           []*batchv1.Job
	CronJobs       []*batchv1.CronJob
	PVCs           []*corev1.PersistentVolumeClaim
	StorageClasses []*storagev1.StorageClass

	// Filled only if the CRDs are installed (see Check.Requires).
	Certificates []Certificate
	Applications []Application

	// VolumeUsage comes from the kubelet stats, keyed "namespace/pvcName".
	VolumeUsage map[string]VolumeUsage

	// Skipped names checks that cannot run in this run because their data
	// could not be collected, with the reason. The engine does not run them
	// and leaves their open problems alone: "no data" is not "all fine".
	Skipped map[string]string
}

// Certificate is the part of a cert-manager Certificate the checks need. The
// kube package converts the unstructured object, so checks never handle
// untyped maps.
type Certificate struct {
	Meta         metav1.ObjectMeta
	Ready        bool
	ReadyReason  string // empty if the object has no Ready condition yet
	ReadyMessage string
	NotAfter     *time.Time // nil if the certificate was not issued yet
}

// Application is the part of an Argo CD Application the checks need.
type Application struct {
	Meta         metav1.ObjectMeta
	HealthStatus string // Healthy, Degraded, Missing, Progressing, ...
	SyncStatus   string // Synced, OutOfSync, ...
}

// VolumeUsage is how full a volume is, as reported by the kubelet.
type VolumeUsage struct {
	UsedBytes     int64
	CapacityBytes int64
}

// Options holds the configurable thresholds and the ignore list.
type Options struct {
	RestartThreshold  int
	PodPendingAfter   time.Duration
	PVCPendingAfter   time.Duration
	CronJobGrace      time.Duration
	IgnoredNamespaces []string
}

// Check couples a stable name with the function that runs it. The name is
// used in findings and, later, to disable a check by configuration.
type Check struct {
	Name string
	// Requires names an optional feature of the cluster that must be present
	// for the check to make sense (one of the Requires* constants); empty
	// for checks that always work.
	Requires string
	Run      func(snap *Snapshot, opts Options) []Finding
}

// Optional cluster features that a check can require. The kube package
// detects them at startup.
const (
	RequiresCertManager = "cert-manager"
	RequiresArgoCD      = "argocd"
)

// All returns every check that exists. It is a function rather than a
// package variable so the package has no global state.
func All() []Check {
	return []Check{
		{Name: checkPodCrashLoop, Run: PodCrashLoop},
		{Name: checkPodImagePull, Run: PodImagePull},
		{Name: checkPodOOMKilled, Run: PodOOMKilled},
		{Name: checkPodRestarts, Run: PodRestarts},
		{Name: checkPodPending, Run: PodPending},
		{Name: checkWorkloadReplicas, Run: WorkloadReplicas},
		{Name: checkNodeNotReady, Run: NodeNotReady},
		{Name: checkNodePressure, Run: NodePressure},
		{Name: checkJobFailed, Run: JobFailed},
		{Name: checkCronJobMissed, Run: CronJobMissed},
		{Name: checkPVCPending, Run: PVCPending},
		{Name: checkPVCUsage, Run: PVCUsage},
		{Name: checkCertNotReady, Requires: RequiresCertManager, Run: CertNotReady},
		{Name: checkCertExpiry, Requires: RequiresCertManager, Run: CertExpiry},
		{Name: checkArgoCDHealth, Requires: RequiresArgoCD, Run: ArgoCDHealth},
		{Name: checkArgoCDSync, Requires: RequiresArgoCD, Run: ArgoCDSync},
	}
}

// Select returns all checks except the disabled ones. A name that matches no
// check is an error, so a typo in the configuration cannot silently leave a
// check running (or silently disable nothing).
func Select(disabled []string) ([]Check, error) {
	all := All()
	var names []string
	for _, c := range all {
		names = append(names, c.Name)
	}
	for _, name := range disabled {
		if !slices.Contains(names, name) {
			return nil, fmt.Errorf("unknown check %q in SMOKEALARM_DISABLED_CHECKS; known checks: %s",
				name, strings.Join(names, ", "))
		}
	}

	var enabled []Check
	for _, c := range all {
		if !slices.Contains(disabled, c.Name) {
			enabled = append(enabled, c)
		}
	}
	return enabled, nil
}

// newFinding fills in the key so every check builds it the same way.
func newFinding(check string, severity Severity, ref ObjectRef, reason, message string) Finding {
	key := strings.Join([]string{check, ref.Kind, ref.Namespace, ref.Name, reason}, "/")
	return Finding{
		Key:      key,
		Check:    check,
		Severity: severity,
		Object:   ref,
		Reason:   reason,
		Message:  message,
	}
}
