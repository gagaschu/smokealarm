package engine

import (
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

// heldBackBy returns the down nodes whose consequences are held back in this
// run. That is only the case while node-not-ready runs: switched off, or unable
// to run, it would leave a failed node without any alarm, and its consequences
// are reported as usual instead.
func heldBackBy(snap *checks.Snapshot, runnable []checks.Check) map[string]bool {
	for _, c := range runnable {
		if c.Name == checks.CheckNodeNotReady {
			return checks.DownNodes(snap)
		}
	}
	return nil
}

// holdBack decides whether a pending problem waits for a down node: as long as
// the finding says it is caused by one (see checks.ApplyNodeCauses). When that
// ends, because the node is back or because the problem can no longer be
// explained by it, the ALERT_AFTER clock starts again: the pods need a moment
// to become ready, and a problem that lasts beyond that is a problem of its
// own. Firing problems are never held back: what was reported stays reported.
func (e *Engine) holdBack(record store.Record, f checks.Finding, now time.Time) store.Record {
	switch {
	case f.CausedBy != "":
		if record.CausedBy == "" {
			e.Logger.Info("finding held back, its node is down", "key", f.Key, "node", f.CausedBy)
		}
		record.CausedBy = f.CausedBy
	case record.CausedBy != "":
		e.Logger.Info("finding no longer held back by its node", "key", f.Key, "node", record.CausedBy)
		record.CausedBy = ""
		record.PendingSince = now
	}
	return record
}

// HeldBack groups the problems that wait for a down node by the node's name.
// The web server uses it too.
func HeldBack(open []store.Record) map[string][]store.Record {
	held := map[string][]store.Record{}
	for _, r := range open {
		if r.State == store.StatePending && r.CausedBy != "" {
			held[r.CausedBy] = append(held[r.CausedBy], r)
		}
	}
	return held
}
