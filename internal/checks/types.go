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
	corev1 "k8s.io/api/core/v1"
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
	Now          time.Time
	Namespaces   []*corev1.Namespace
	Pods         []*corev1.Pod
	Events       []*corev1.Event // Warning events only
	Deployments  []*appsv1.Deployment
	StatefulSets []*appsv1.StatefulSet
	DaemonSets   []*appsv1.DaemonSet
	Nodes        []*corev1.Node
}

// Options holds the configurable thresholds and the ignore list.
type Options struct {
	RestartThreshold  int
	PodPendingAfter   time.Duration
	IgnoredNamespaces []string
}

// Check couples a stable name with the function that runs it. The name is
// used in findings and, later, to disable a check by configuration.
type Check struct {
	Name string
	Run  func(snap *Snapshot, opts Options) []Finding
}

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
