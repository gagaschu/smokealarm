package checks

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtr(b bool) *bool { return &b }

// newJob builds a Job in namespace "batch". owner may be empty for a
// standalone Job. A failed job has the Failed=True condition.
func newJob(name, owner string, created time.Time, failed bool) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "batch",
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
		},
	}
	if owner != "" {
		job.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: owner, Controller: boolPtr(true)}}
	}
	if failed {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}}
	}
	return job
}

func newCronJob(name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "batch",
			Name:              name,
			CreationTimestamp: metav1.NewTime(testNow.Add(-24 * time.Hour)),
		},
		Spec: batchv1.CronJobSpec{Schedule: "*/5 * * * *"},
	}
}

func TestJobFailedStandalone(t *testing.T) {
	tests := []struct {
		name string
		job  *batchv1.Job
		want []string
	}{
		{"failed", newJob("migrate", "", testNow, true), []string{"job-failed/Job/batch/migrate/JobFailed"}},
		{"succeeded or running", newJob("migrate", "", testNow, false), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Jobs: []*batchv1.Job{tt.job}}
			wantFindings(t, JobFailed(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestJobFailedMessage(t *testing.T) {
	snap := &Snapshot{Now: testNow, Jobs: []*batchv1.Job{newJob("migrate", "", testNow, true)}}
	got := JobFailed(snap, testOptions())
	want := "job migrate failed: BackoffLimitExceeded: Job has reached the specified backoff limit"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestJobFailedCronJobOnlyNewestCounts(t *testing.T) {
	const key = "job-failed/CronJob/batch/backup/LastJobFailed"
	old := testNow.Add(-2 * time.Hour)
	recent := testNow.Add(-time.Hour)

	tests := []struct {
		name string
		jobs []*batchv1.Job
		want []string
	}{
		{"newest failed", []*batchv1.Job{newJob("backup-1", "backup", old, false), newJob("backup-2", "backup", recent, true)}, []string{key}},
		{"old failed, newest succeeded", []*batchv1.Job{newJob("backup-1", "backup", old, true), newJob("backup-2", "backup", recent, false)}, nil},
		{"order in the list does not matter", []*batchv1.Job{newJob("backup-2", "backup", recent, false), newJob("backup-1", "backup", old, true)}, nil},
		{"same second, higher name wins", []*batchv1.Job{newJob("backup-b", "backup", recent, true), newJob("backup-a", "backup", recent, false)}, []string{key}},
		{"several failed runs give one finding", []*batchv1.Job{newJob("backup-1", "backup", old, true), newJob("backup-2", "backup", recent, true)}, []string{key}},
		{"cronjob is gone", []*batchv1.Job{newJob("other-1", "other", recent, true)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Jobs: tt.jobs, CronJobs: []*batchv1.CronJob{newCronJob("backup")}}
			wantFindings(t, JobFailed(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestJobFailedRespectsIgnore(t *testing.T) {
	annotated := map[string]string{IgnoreAnnotation: "true"}

	ignoredCronJob := newCronJob("backup")
	ignoredCronJob.Annotations = annotated
	ignoredJob := newJob("backup-1", "backup", testNow, true)
	ignoredJob.Annotations = annotated
	ignoredStandalone := newJob("migrate", "", testNow, true)
	ignoredStandalone.Annotations = annotated

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"cronjob job not ignored", &Snapshot{Jobs: []*batchv1.Job{newJob("backup-1", "backup", testNow, true)}, CronJobs: []*batchv1.CronJob{newCronJob("backup")}}, testOptions(), 1},
		{"cronjob annotated", &Snapshot{Jobs: []*batchv1.Job{newJob("backup-1", "backup", testNow, true)}, CronJobs: []*batchv1.CronJob{ignoredCronJob}}, testOptions(), 0},
		{"job annotated", &Snapshot{Jobs: []*batchv1.Job{ignoredJob}, CronJobs: []*batchv1.CronJob{newCronJob("backup")}}, testOptions(), 0},
		{"standalone annotated", &Snapshot{Jobs: []*batchv1.Job{ignoredStandalone}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{Jobs: []*batchv1.Job{newJob("migrate", "", testNow, true)}}, Options{IgnoredNamespaces: []string{"batch"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Now = testNow
			if got := len(JobFailed(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

func cronOptions() Options {
	return Options{CronJobGrace: 5 * time.Minute}
}

func TestCronJobMissedBoundary(t *testing.T) {
	// Schedule */5, last run 12:00 -> next expected 12:05, grace until 12:10.
	last := metav1.NewTime(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	const key = "cronjob-missed/CronJob/batch/backup/MissedSchedule"

	tests := []struct {
		name string
		now  time.Time
		want []string
	}{
		{"before the next run", time.Date(2026, 11, 3, 12, 4, 0, 0, time.UTC), nil},
		{"late but within grace", time.Date(2026, 11, 3, 12, 9, 59, 0, time.UTC), nil},
		{"just past grace", time.Date(2026, 11, 3, 12, 10, 1, 0, time.UTC), []string{key}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := newCronJob("backup")
			cj.Status.LastScheduleTime = &last
			snap := &Snapshot{Now: tt.now, CronJobs: []*batchv1.CronJob{cj}}
			wantFindings(t, CronJobMissed(snap, cronOptions()), tt.want, Warning)
		})
	}
}

func TestCronJobMissedNeverRan(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		want    int
	}{
		{"created long ago, never ran", now.Add(-time.Hour), 1},
		{"just created", now.Add(-time.Minute), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := newCronJob("backup")
			cj.CreationTimestamp = metav1.NewTime(tt.created)
			snap := &Snapshot{Now: now, CronJobs: []*batchv1.CronJob{cj}}
			got := CronJobMissed(snap, cronOptions())
			if len(got) != tt.want {
				t.Fatalf("got %+v, want %d findings", got, tt.want)
			}
			if tt.want == 1 && got[0].Message == "" {
				t.Error("empty message")
			}
		})
	}
}

func TestCronJobMissedSkips(t *testing.T) {
	overdue := func() *batchv1.CronJob {
		cj := newCronJob("backup")
		last := metav1.NewTime(testNow.Add(-time.Hour))
		cj.Status.LastScheduleTime = &last
		return cj
	}
	suspended := overdue()
	suspended.Spec.Suspend = boolPtr(true)
	forbidRunning := overdue()
	forbidRunning.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	forbidRunning.Status.Active = []corev1.ObjectReference{{Name: "backup-1"}}
	forbidIdle := overdue()
	forbidIdle.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	allowRunning := overdue()
	allowRunning.Status.Active = []corev1.ObjectReference{{Name: "backup-1"}}
	annotated := overdue()
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}

	tests := []struct {
		name string
		cj   *batchv1.CronJob
		want int
	}{
		{"overdue", overdue(), 1},
		{"suspended", suspended, 0},
		{"Forbid with a running job", forbidRunning, 0},
		{"Forbid without a running job", forbidIdle, 1},
		{"Allow with a running job", allowRunning, 1},
		{"ignored by annotation", annotated, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, CronJobs: []*batchv1.CronJob{tt.cj}}
			if got := len(CronJobMissed(snap, cronOptions())); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

func TestCronJobMissedTimeZone(t *testing.T) {
	// 09:00 in Berlin is 08:00 UTC in November (CET). At 08:30 UTC today's
	// run is 30 minutes late. Read as UTC, the same schedule is not due
	// before 09:00, so only the timeZone field makes it a miss.
	now := time.Date(2026, 11, 3, 8, 30, 0, 0, time.UTC)
	berlin := "Europe/Berlin"

	tests := []struct {
		name    string
		tz      *string
		lastRun time.Time // yesterday's run, in the zone the schedule is read in
		want    int
	}{
		{"with timeZone", &berlin, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), 1},
		{"without timeZone", nil, time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := newCronJob("daily")
			cj.Spec.Schedule = "0 9 * * *"
			cj.Spec.TimeZone = tt.tz
			last := metav1.NewTime(tt.lastRun)
			cj.Status.LastScheduleTime = &last

			got := CronJobMissed(&Snapshot{Now: now, CronJobs: []*batchv1.CronJob{cj}}, cronOptions())
			if len(got) != tt.want {
				t.Errorf("got %+v, want %d findings", got, tt.want)
			}
		})
	}
}

func TestCronJobMissedInvalidSchedule(t *testing.T) {
	cj := newCronJob("broken")
	cj.Spec.Schedule = "every now and then"
	got := CronJobMissed(&Snapshot{Now: testNow, CronJobs: []*batchv1.CronJob{cj}}, cronOptions())
	if len(got) != 1 || got[0].Reason != "InvalidSchedule" {
		t.Errorf("got %+v, want one InvalidSchedule finding", got)
	}

	unknownZone := newCronJob("zone")
	tz := "Mars/Olympus"
	unknownZone.Spec.TimeZone = &tz
	got = CronJobMissed(&Snapshot{Now: testNow, CronJobs: []*batchv1.CronJob{unknownZone}}, cronOptions())
	if len(got) != 1 || got[0].Reason != "InvalidSchedule" {
		t.Errorf("unknown zone: got %+v, want one InvalidSchedule finding", got)
	}
}
