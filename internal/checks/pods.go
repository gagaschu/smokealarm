package checks

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Check names. They appear in findings and are the ids for disabling checks.
const (
	checkPodCrashLoop = "pod-crashloop"
	checkPodImagePull = "pod-image-pull"
	checkPodOOMKilled = "pod-oomkilled"
	checkPodRestarts  = "pod-restarts"
	checkPodPending   = "pod-pending"
)

// Fixed time windows from the specification. They are not configurable on
// purpose: smokealarm should work without tuning.
const (
	oomWindow     = 15 * time.Minute
	restartWindow = time.Hour
)

// Waiting reasons reported by the kubelet.
const (
	reasonCrashLoop = "CrashLoopBackOff"
	reasonImagePull = "ImagePullBackOff"
	reasonErrImage  = "ErrImagePull"
	reasonOOMKilled = "OOMKilled"
)

// PodCrashLoop reports containers that keep crashing and are waiting in
// CrashLoopBackOff. The workload is effectively down while this lasts, so
// the severity is critical.
func PodCrashLoop(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		for _, cs := range containerStatuses(pod) {
			if cs.State.Waiting == nil || cs.State.Waiting.Reason != reasonCrashLoop {
				continue
			}
			msg := fmt.Sprintf("container %s keeps crashing (%d restarts)", cs.Name, cs.RestartCount)
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg += fmt.Sprintf(", last exit code %d (%s)", t.ExitCode, t.Reason)
			}
			findings = append(findings, newFinding(checkPodCrashLoop, Critical, podRef(pod), reasonCrashLoop, msg))
			break // one finding per pod is enough
		}
	}
	return findings
}

// PodImagePull reports containers whose image cannot be pulled (wrong name,
// missing credentials, registry down). The pod will never start, so the
// severity is critical.
//
// The kubelet alternates between ErrImagePull and ImagePullBackOff. Both map
// to the same reason, otherwise the finding key would flip every few seconds.
func PodImagePull(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		if cs := imagePullProblem(pod); cs != nil {
			msg := fmt.Sprintf("container %s cannot pull image %s: %s", cs.Name, cs.Image, cs.State.Waiting.Message)
			findings = append(findings, newFinding(checkPodImagePull, Critical, podRef(pod), reasonImagePull, msg))
		}
	}
	return findings
}

// PodOOMKilled reports containers that were killed for exceeding their memory
// limit within the last 15 minutes. The container restarts, so this is only a
// warning; the limit is probably too low.
func PodOOMKilled(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		for _, cs := range containerStatuses(pod) {
			t := lastTermination(cs)
			if t == nil || t.Reason != reasonOOMKilled || snap.Now.Sub(t.FinishedAt.Time) > oomWindow {
				continue
			}
			msg := fmt.Sprintf("container %s was OOMKilled at %s", cs.Name, opts.formatTime(t.FinishedAt.Time))
			findings = append(findings, newFinding(checkPodOOMKilled, Warning, podRef(pod), reasonOOMKilled, msg))
			break
		}
	}
	return findings
}

// PodRestarts reports containers with more restarts than the configured
// threshold, but only if the last restart was within the past hour. Without
// the time window, a pod that was unstable last week would alarm forever.
// Severity is warning.
func PodRestarts(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		for _, cs := range containerStatuses(pod) {
			t := lastTermination(cs)
			if int(cs.RestartCount) <= opts.RestartThreshold || t == nil || snap.Now.Sub(t.FinishedAt.Time) > restartWindow {
				continue
			}
			msg := fmt.Sprintf("container %s restarted %d times, last exit code %d (%s)",
				cs.Name, cs.RestartCount, t.ExitCode, t.Reason)
			findings = append(findings, newFinding(checkPodRestarts, Warning, podRef(pod), "TooManyRestarts", msg))
			break
		}
	}
	return findings
}

// PodPending reports pods that stay in phase Pending longer than the
// configured time, typically because they cannot be scheduled. Severity is
// warning.
//
// The reason is always "PodPending"; the detail (for example Unschedulable)
// goes into the message so the finding key stays stable. Pods with an image
// pull problem are left to PodImagePull to avoid a double alarm.
func PodPending(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if pod.Status.Phase != corev1.PodPending || isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		age := snap.Now.Sub(pod.CreationTimestamp.Time)
		if age <= opts.PodPendingAfter || imagePullProblem(pod) != nil {
			continue
		}
		msg := fmt.Sprintf("pending for %s: %s", age.Round(time.Second), pendingDetail(snap, pod))
		findings = append(findings, newFinding(checkPodPending, Warning, podRef(pod), "PodPending", msg))
	}
	return findings
}

// pendingDetail explains why a pod is pending: first the scheduler's
// condition, then the newest warning event.
func pendingDetail(snap *Snapshot, pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		}
	}
	if e := latestWarning(snap, podRef(pod)); e != nil {
		return fmt.Sprintf("%s: %s", e.Reason, e.Message)
	}
	return "no details available"
}

// imagePullProblem returns the first container that cannot pull its image.
func imagePullProblem(pod *corev1.Pod) *corev1.ContainerStatus {
	for _, cs := range containerStatuses(pod) {
		w := cs.State.Waiting
		if w != nil && (w.Reason == reasonImagePull || w.Reason == reasonErrImage) {
			return &cs
		}
	}
	return nil
}

// lastTermination returns how the container last ended: the previous run if
// it is restarting, otherwise the current state if it is terminated.
func lastTermination(cs corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if t := cs.LastTerminationState.Terminated; t != nil {
		return t
	}
	return cs.State.Terminated
}

// containerStatuses returns init and regular container statuses together.
func containerStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	all := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	return append(all, pod.Status.ContainerStatuses...)
}

func podRef(pod *corev1.Pod) ObjectRef {
	return ObjectRef{Kind: KindPod, Namespace: pod.Namespace, Name: pod.Name}
}
