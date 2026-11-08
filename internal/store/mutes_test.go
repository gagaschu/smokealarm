package store

import (
	"context"
	"testing"
	"time"
)

func TestMuteTargetAndRequestUnmute(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0, t1 := testNow, testNow.Add(time.Minute)

	for i, want := range []bool{true, false} {
		changed, err := s.MuteTarget(ctx, "mail", t0)
		if err != nil || changed != want {
			t.Fatalf("MuteTarget #%d = %v, %v, want %v", i+1, changed, err, want)
		}
	}
	mutes, err := s.TargetMutes(ctx)
	if err != nil {
		t.Fatalf("TargetMutes: %v", err)
	}
	if m := mutes["mail"]; !m.MutedAt.Equal(t0) || m.UnmuteRequestedAt != nil {
		t.Errorf("mail = %+v, want muted at %v without a request", m, t0)
	}

	changed, err := s.RequestUnmute(ctx, "mail", t1)
	if err != nil || !changed {
		t.Fatalf("RequestUnmute(mail) = %v, %v, want true", changed, err)
	}
	mutes, _ = s.TargetMutes(ctx)
	if m := mutes["mail"]; m.UnmuteRequestedAt == nil || !m.UnmuteRequestedAt.Equal(t1) {
		t.Errorf("UnmuteRequestedAt = %v, want %v", m.UnmuteRequestedAt, t1)
	}
	changed, err = s.RequestUnmute(ctx, "slack", t1)
	if err != nil || changed {
		t.Errorf("RequestUnmute(slack) = %v, %v, want false (it is not muted)", changed, err)
	}
}

func TestMuteAgainCancelsTheUnmuteRequest(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0, t1, t2 := testNow, testNow.Add(time.Minute), testNow.Add(2*time.Minute)
	if _, err := s.MuteTarget(ctx, "mail", t0); err != nil {
		t.Fatalf("MuteTarget: %v", err)
	}
	if _, err := s.RequestUnmute(ctx, "mail", t1); err != nil {
		t.Fatalf("RequestUnmute: %v", err)
	}

	changed, err := s.MuteTarget(ctx, "mail", t2)
	if err != nil || !changed {
		t.Fatalf("MuteTarget = %v, %v, want true", changed, err)
	}
	mutes, _ := s.TargetMutes(ctx)
	if m := mutes["mail"]; !m.MutedAt.Equal(t0) || m.UnmuteRequestedAt != nil {
		t.Errorf("mail = %+v, want muted_at %v and no request", m, t0)
	}
}

func TestFinishUnmuteWithoutRequestDoesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := firingRecord(t, s, "a")
	mustMarkFiring(t, s, "mail", id, testNow)
	if _, err := s.MuteTarget(ctx, "mail", testNow); err != nil {
		t.Fatalf("MuteTarget: %v", err)
	}

	finished, err := s.FinishUnmute(ctx, "mail", []int64{id}, testNow.Add(time.Hour))
	if err != nil || finished {
		t.Fatalf("FinishUnmute = %v, %v, want false", finished, err)
	}
	if mutes, _ := s.TargetMutes(ctx); len(mutes) != 1 {
		t.Errorf("mutes = %v, want the row to stay", mutes)
	}
	if keys := unnotifiedKeys(t, s, "mail"); keys != "" {
		t.Errorf("Unnotified = %q, want nothing: the markers must not change", keys)
	}
}

func TestFinishUnmuteClearsFiringAndSilencesResolved(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	firingID := firingRecord(t, s, "firing")
	mustMarkFiring(t, s, "mail", firingID, testNow)
	mustMarkFiring(t, s, "slack", firingID, testNow)

	resolved := newRecord("resolved", StateResolved, testNow)
	firingAt, resolvedAt := testNow, testNow.Add(time.Hour)
	resolved.FiringAt, resolved.ResolvedAt = &firingAt, &resolvedAt
	resolvedID := insertedID(t, s, resolved)
	mustMarkFiring(t, s, "mail", resolvedID, testNow)

	if _, err := s.MuteTarget(ctx, "mail", testNow); err != nil {
		t.Fatalf("MuteTarget: %v", err)
	}
	if _, err := s.RequestUnmute(ctx, "mail", testNow.Add(time.Minute)); err != nil {
		t.Fatalf("RequestUnmute: %v", err)
	}
	finished, err := s.FinishUnmute(ctx, "mail", []int64{firingID}, testNow.Add(2*time.Hour))
	if err != nil || !finished {
		t.Fatalf("FinishUnmute = %v, %v, want true", finished, err)
	}

	if keys := unnotifiedKeys(t, s, "mail"); keys != "firing" {
		t.Errorf("mail: Unnotified = %q, want only the firing problem", keys)
	}
	if keys := unnotifiedKeys(t, s, "slack"); keys != "resolved" {
		t.Errorf("slack: Unnotified = %q, want the resolved problem (slack was not muted)", keys)
	}
	if mutes, _ := s.TargetMutes(ctx); len(mutes) != 0 {
		t.Errorf("mutes = %v, want none", mutes)
	}
}

func TestRemovedTargetForgetsItsMute(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for _, names := range [][]string{{"mail", "slack"}, {"mail"}} {
		if _, _, err := s.SyncTargets(ctx, names); err != nil {
			t.Fatalf("SyncTargets(%v): %v", names, err)
		}
		if names[len(names)-1] == "slack" {
			if _, err := s.MuteTarget(ctx, "slack", testNow); err != nil {
				t.Fatalf("MuteTarget: %v", err)
			}
		}
	}
	if _, _, err := s.SyncTargets(ctx, []string{"mail", "slack"}); err != nil {
		t.Fatalf("SyncTargets: %v", err)
	}

	mutes, err := s.TargetMutes(ctx)
	if err != nil || len(mutes) != 0 {
		t.Errorf("TargetMutes = %v, %v, want none: a target that comes back starts afresh", mutes, err)
	}
}
