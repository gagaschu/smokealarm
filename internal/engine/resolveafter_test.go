package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// firingWithResolveAfter brings problem "a" to the point where it was reported,
// with five minutes of RESOLVE_AFTER.
func firingWithResolveAfter(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.wantSent(1)
	return h
}

// A crash loop: the finding is there while the container waits for its back-off
// and gone while it runs. It sent one message per end and one per start.
func TestCrashLoopIsOneAlarmAndNotAPairPerRestart(t *testing.T) {
	h := firingWithResolveAfter(t)

	for i := 0; i < 5; i++ {
		h.findings = nil
		h.mustRun(time.Minute)
		h.findings = []checks.Finding{finding("a", checks.Critical)}
		h.mustRun(5 * time.Minute)
	}
	h.wantSent(1)
	if h.countRows("resolved") != 0 {
		t.Errorf("%d resolved rows, want none", h.countRows("resolved"))
	}
}

func TestProblemResolvesWhenGoneForResolveAfter(t *testing.T) {
	h := firingWithResolveAfter(t)
	gone := h.now.Add(30 * time.Second)

	h.findings = nil
	h.mustRun(30 * time.Second)
	h.mustRun(4*time.Minute + 59*time.Second)
	h.wantSent(1)
	if h.countRows("firing") != 1 {
		t.Fatalf("%d firing rows just below RESOLVE_AFTER, want 1", h.countRows("firing"))
	}

	h.mustRun(time.Second)
	h.wantSent(2)
	h.wantLastResolved("a")
	last := h.notifier.sent[1].Resolved[0]
	if !last.ResolvedAt.Equal(gone) {
		t.Errorf("ResolvedAt = %v, want the run it was first gone in, %v", last.ResolvedAt, gone)
	}
}

func TestComingBackStartsTheResolveAfterWaitAgain(t *testing.T) {
	h := firingWithResolveAfter(t)

	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(4 * time.Minute) // nearly over
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(30 * time.Second)
	h.findings = nil
	h.mustRun(30 * time.Second)
	h.mustRun(4 * time.Minute)
	h.wantSent(1)

	h.mustRun(time.Minute)
	h.wantLastResolved("a")
}

func TestResolveAfterWaitSurvivesARestart(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.findings = nil
	h.mustRun(time.Minute)

	h.restart()
	h.engine.ResolveAfter = 5 * time.Minute
	h.mustRun(5 * time.Minute)
	h.wantLastResolved("a")
}

func TestPendingProblemIsDroppedAtOnceDespiteResolveAfter(t *testing.T) {
	h := newHarness(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.findings = nil
	h.mustRun(30 * time.Second)
	if n := h.countRows(""); n != 0 {
		t.Errorf("%d rows, want the pending problem gone", n)
	}
}

func TestNoResolveAfterResolvesInTheSameRun(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(30 * time.Second)
	h.wantLastResolved("a")
}

func TestNoReminderWhileAProblemIsGone(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.engine.CriticalRepeatInterval = time.Minute

	h.findings = nil
	h.mustRun(2 * time.Minute)
	h.mustRun(2 * time.Minute)
	h.wantSent(1)
}

// A message that failed is not sent later about a problem that is gone by then.
func TestFirstMessageIsNotSentForAProblemThatIsGone(t *testing.T) {
	h := newHarness(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.notifier.fail = true
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	if err := h.run(2 * time.Minute); err == nil {
		t.Fatal("expected the failed send to be returned")
	}

	h.notifier.fail = false
	h.findings = nil
	h.mustRun(30 * time.Second)
	h.wantSent(0)

	h.mustRun(5 * time.Minute)
	h.wantSent(0) // nobody heard the start
}

func TestOpenRecordsSayWhenAProblemWasLastThere(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.findings = nil
	h.mustRun(30 * time.Second)
	gone := h.now

	h.mustRun(30 * time.Second)
	records, err := h.engine.Store.OpenRecords(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	if records[0].GoneSince == nil || !records[0].GoneSince.Equal(gone) {
		t.Errorf("GoneSince = %v, want %v", records[0].GoneSince, gone)
	}

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(30 * time.Second)
	records, err = h.engine.Store.OpenRecords(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	if records[0].GoneSince != nil {
		t.Errorf("GoneSince = %v, want none once it is back", records[0].GoneSince)
	}
}

// What a check whose state is unknown says nothing about does not wait.
func TestSkippedCheckDoesNotStartTheResolveAfterWait(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.skipped = map[string]string{"fake": "no data"}
	h.findings = nil
	h.mustRun(time.Hour)
	if strings.Contains(h.logs.String(), "finding no longer seen") {
		t.Error("a skipped check started the wait")
	}
	h.skipped = nil
	h.mustRun(30 * time.Second)
	h.wantSent(1) // the wait starts now
	h.mustRun(5 * time.Minute)
	h.wantLastResolved("a")
}

// At the end of a downtime a problem that is absent counts as gone during the
// downtime: nothing is sent when it resolves.
func TestDowntimeEndingDuringTheWaitStaysSilent(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.setDowntime("a", 2*time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(time.Minute) // the downtime ends here
	h.mustRun(30 * time.Second)
	h.wantSent(1)

	h.mustRun(5 * time.Minute)
	h.wantSent(1)
	if h.countRows("resolved") != 1 {
		t.Errorf("%d resolved rows, want 1", h.countRows("resolved"))
	}
}

// If it comes back after that, it is reported again, as after any downtime.
func TestProblemBackAfterADowntimeEndedDuringTheWaitIsReported(t *testing.T) {
	h := firingWithResolveAfter(t)
	h.setDowntime("a", 2*time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(time.Minute)
	h.wantSent(1)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(30 * time.Second)
	h.wantSent(2)
	h.wantLastNew("a")
}
