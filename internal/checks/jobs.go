package checks

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	checkJobFailed     = "job-failed"
	checkCronJobMissed = "cronjob-missed"
)

// JobFailed reports Jobs that have failed (condition Failed=True): the work
// did not get done. Severity is warning.
//
// A CronJob creates a new Job per run and keeps old ones around, so old
// failures would alarm forever. For Jobs owned by a CronJob only the newest
// Job counts, and the finding is reported on the CronJob (reason
// LastJobFailed). That keeps the finding key stable from run to run and
// resolves it as soon as one run succeeds. Standalone Jobs are reported
// as themselves (reason JobFailed).
func JobFailed(snap *Snapshot, opts Options) []Finding {
	var findings []Finding

	newestByCronJob := map[string]*batchv1.Job{}
	for _, job := range snap.Jobs {
		owner := cronJobOwner(job)
		if owner == "" {
			if f, ok := standaloneJobFinding(snap, opts, job); ok {
				findings = append(findings, f)
			}
			continue
		}
		key := job.Namespace + "/" + owner
		if current := newestByCronJob[key]; current == nil || isNewer(job, current) {
			newestByCronJob[key] = job
		}
	}

	// Walk the CronJobs (not the map) so the output order is stable. Jobs
	// whose CronJob is gone are skipped: nobody schedules them anymore.
	for _, cj := range snap.CronJobs {
		job := newestByCronJob[cj.Namespace+"/"+cj.Name]
		if job == nil || isIgnored(snap, opts, cj.ObjectMeta) || isIgnored(snap, opts, job.ObjectMeta) {
			continue
		}
		if cond := failedCondition(job); cond != nil {
			ref := ObjectRef{Kind: KindCronJob, Namespace: cj.Namespace, Name: cj.Name}
			findings = append(findings, newFinding(checkJobFailed, Warning, ref, "LastJobFailed", failedMessage(job, cond)))
		}
	}
	return findings
}

func standaloneJobFinding(snap *Snapshot, opts Options, job *batchv1.Job) (Finding, bool) {
	cond := failedCondition(job)
	if cond == nil || isIgnored(snap, opts, job.ObjectMeta) {
		return Finding{}, false
	}
	ref := ObjectRef{Kind: KindJob, Namespace: job.Namespace, Name: job.Name}
	return newFinding(checkJobFailed, Warning, ref, "JobFailed", failedMessage(job, cond)), true
}

func failedMessage(job *batchv1.Job, cond *batchv1.JobCondition) string {
	return fmt.Sprintf("job %s failed: %s: %s", job.Name, cond.Reason, cond.Message)
}

// CronJobMissed reports CronJobs that should have started a run by now but
// did not, for example because the controller was down or the CronJob
// controller could not create the Job. Severity is warning.
//
// The expected run is the first scheduled time after the last run (or after
// creation if it never ran). It counts as missed once it is more than the
// grace period in the past. Suspended CronJobs are skipped on purpose, and so
// are CronJobs with concurrencyPolicy Forbid that still have a running Job,
// because the controller skips runs in that case by design. A schedule that
// never comes true (February 31, a common way to write "only by hand") has no
// expected run, so nothing can be missed.
func CronJobMissed(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, cj := range snap.CronJobs {
		if isSuspended(cj) || isIgnored(snap, opts, cj.ObjectMeta) {
			continue
		}
		if cj.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(cj.Status.Active) > 0 {
			continue
		}
		ref := ObjectRef{Kind: KindCronJob, Namespace: cj.Namespace, Name: cj.Name}

		schedule, err := parseSchedule(cj, opts.CronJobTimeZone)
		if err != nil {
			msg := fmt.Sprintf("cannot parse schedule %q: %v", cj.Spec.Schedule, err)
			findings = append(findings, newFinding(checkCronJobMissed, Warning, ref, "InvalidSchedule", msg))
			continue
		}

		since, lastRun := cj.CreationTimestamp.Time, "never"
		if last := cj.Status.LastScheduleTime; last != nil {
			since, lastRun = last.Time, last.Format(time.RFC3339)
		}
		expected := schedule.Next(since)
		if expected.IsZero() {
			continue
		}
		if snap.Now.After(expected.Add(opts.CronJobGrace)) {
			msg := fmt.Sprintf("expected run at %s, last run: %s", expected.UTC().Format(time.RFC3339), lastRun)
			findings = append(findings, newFinding(checkCronJobMissed, Warning, ref, "MissedSchedule", msg))
		}
	}
	return findings
}

// parseSchedule turns the cron expression into a schedule. The robfig parser
// is the same one Kubernetes uses; a time zone is passed as a CRON_TZ prefix.
//
// Without spec.timeZone the controller reads the schedule in its own local
// time, which smokealarm cannot see from the API. On k3s that is the time zone
// of the host. defaultZone says what it is; empty means UTC.
func parseSchedule(cj *batchv1.CronJob, defaultZone string) (cron.Schedule, error) {
	zone := defaultZone
	if tz := cj.Spec.TimeZone; tz != nil && *tz != "" {
		zone = *tz
	}
	if zone == "" {
		return cron.ParseStandard(cj.Spec.Schedule)
	}
	return cron.ParseStandard("CRON_TZ=" + zone + " " + cj.Spec.Schedule)
}

func isSuspended(cj *batchv1.CronJob) bool {
	return cj.Spec.Suspend != nil && *cj.Spec.Suspend
}

// cronJobOwner returns the name of the CronJob that created the job, or "".
func cronJobOwner(job *batchv1.Job) string {
	owner := metav1.GetControllerOf(job)
	if owner == nil || owner.Kind != "CronJob" {
		return ""
	}
	return owner.Name
}

// failedCondition returns the Failed condition if it is true, otherwise nil.
func failedCondition(job *batchv1.Job) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// isNewer orders jobs by creation time; the name breaks ties because
// timestamps only have second resolution.
func isNewer(a, b *batchv1.Job) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name > b.Name
	}
	return a.CreationTimestamp.After(b.CreationTimestamp.Time)
}
