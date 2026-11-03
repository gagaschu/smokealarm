package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

func TestLogSend(t *testing.T) {
	var out bytes.Buffer
	n := &Log{Logger: slog.New(slog.NewTextHandler(&out, nil))}

	finding := checks.Finding{
		Key: "pod-crashloop/Pod/media/jellyfin/CrashLoopBackOff", Severity: checks.Critical,
		Reason: "CrashLoopBackOff", Message: "container app keeps crashing",
	}
	err := n.Send(context.Background(), Notification{
		New:       []Alert{{Finding: finding, Since: time.Now()}},
		Reminders: []Alert{{Finding: finding, Since: time.Now()}},
		Resolved:  []Alert{{Finding: finding, Since: time.Now(), ResolvedAt: time.Now()}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	log := out.String()
	for _, want := range []string{"notification: problem", "notification: still critical", "notification: resolved", "pod-crashloop/Pod/media/jellyfin", "severity=critical"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
	if n.Name() != "log" {
		t.Errorf("Name = %q", n.Name())
	}
}
