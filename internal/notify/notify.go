// Package notify delivers notifications about state changes. The Notifier
// interface has one implementation per target (log, mail, webhook, ntfy,
// Telegram, Slack, Teams), so the engine does not care where messages go.
// Each target lives in its own file.
package notify

import (
	"context"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Notifier sends one combined notification to one target. Implementations
// return an error if the message could not be delivered, so the engine can try
// again. Errors must not contain secrets: no token, no password, no webhook URL.
type Notifier interface {
	// Name identifies the target. It is stored in the database to remember
	// what this target was told, so it must stay the same between restarts
	// and differ between targets ("mail", "slack", "telegram:-1001234").
	Name() string
	// Description is how the target is shown on the status page and in the
	// log, without secrets, e.g. "ntfy (ntfy.example.org)".
	Description() string
	Send(ctx context.Context, n Notification) error
}

// Notification bundles everything to report since the last successful
// notification: one message per run, not one per problem.
type Notification struct {
	New       []Alert // problems that started firing, critical first
	Reminders []Alert // critical problems reported before that are still open
	Resolved  []Alert // problems that went away
	BaseURL   string  // link to the web UI, empty if not configured
}

// Alert is a finding plus the times a reader wants to see.
type Alert struct {
	checks.Finding
	Since      time.Time // when the problem was first seen
	ResolvedAt time.Time // zero for new problems
}
