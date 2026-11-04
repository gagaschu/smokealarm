package notify

import (
	"context"
	"log/slog"
)

// Log writes notifications to the log. It is used when no other notifier is
// configured, and it makes state changes visible while developing.
type Log struct {
	Logger *slog.Logger
}

// Name identifies the target.
func (l *Log) Name() string { return "log" }

// Description is shown on the status page.
func (l *Log) Description() string { return "log only" }

// Send logs one line per alert. It never fails.
func (l *Log) Send(_ context.Context, n Notification) error {
	for _, a := range n.New {
		l.Logger.Warn("notification: problem",
			"key", a.Key,
			"severity", a.Severity,
			"reason", a.Reason,
			"message", a.Message,
			"since", a.Since,
		)
	}
	for _, a := range n.Reminders {
		l.Logger.Warn("notification: still critical",
			"key", a.Key,
			"severity", a.Severity,
			"reason", a.Reason,
			"message", a.Message,
			"since", a.Since,
		)
	}
	for _, a := range n.Resolved {
		l.Logger.Info("notification: resolved",
			"key", a.Key,
			"severity", a.Severity,
			"reason", a.Reason,
			"since", a.Since,
			"resolved_at", a.ResolvedAt,
		)
	}
	return nil
}
