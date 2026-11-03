// Package notify delivers notifications about state changes. The Notifier
// interface has more than one implementation (log now, mail next, later
// Telegram or a webhook), so the engine does not care where messages go.
package notify

import (
	"context"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Notifier sends one combined notification. Implementations return an error
// if the message could not be delivered, so the engine can try again.
type Notifier interface {
	Name() string
	Send(ctx context.Context, n Notification) error
}

// Notification bundles everything that changed since the last successful
// notification: one message per run, not one per problem.
type Notification struct {
	New      []Alert // problems that started firing
	Resolved []Alert // problems that went away
}

// Alert is a finding plus the times a reader wants to see.
type Alert struct {
	checks.Finding
	Since      time.Time // when the problem was first seen
	ResolvedAt time.Time // zero for new problems
}
