package notify

import (
	"context"
	"net/http"
	"strings"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// ntfyLimit is a little below the 4096 bytes ntfy accepts as a message; a
// longer one would arrive as an attachment.
const ntfyLimit = 4000

// Ntfy publishes the notification to an ntfy topic, which phones and browsers
// can subscribe to. The server can be ntfy.sh or a self-hosted one.
type Ntfy struct {
	Server string // e.g. "https://ntfy.sh", without a trailing path
	// Topic is a secret on a public server: anybody who knows it can read
	// along. It is therefore not shown anywhere.
	Topic  string
	Token  string // optional access token (Authorization: Bearer)
	Client *http.Client
	// Templates are the user's message templates; nil means the built-in ones.
	Templates *Templates
}

// Name identifies the target.
func (t *Ntfy) Name() string { return "ntfy" }

// Description shows neither the server nor the topic.
func (t *Ntfy) Description() string { return "ntfy" }

// Send publishes the short text, with the subject as its title. A
// *FallbackError means the message went out with a built-in template.
func (t *Ntfy) Send(ctx context.Context, n Notification) error {
	subject, subjectFallback := t.Templates.subject("ntfy", n)
	text, textFallback := t.Templates.chatBody("ntfy", n, ntfyLimit)
	priority, tags := ntfyPriority(n)
	headers := map[string]string{
		"Title":    subject,
		"Priority": priority,
		"Tags":     tags,
	}
	if n.BaseURL != "" {
		headers["Click"] = n.BaseURL
	}
	if t.Token != "" {
		headers["Authorization"] = "Bearer " + t.Token
	}
	err := post(ctx, t.Client, request{
		target:      "ntfy",
		url:         strings.TrimRight(t.Server, "/") + "/" + t.Topic,
		contentType: "text/plain; charset=utf-8",
		body:        []byte(text),
		headers:     headers,
		secrets:     []string{t.Topic, t.Token},
	})
	if err != nil {
		return err
	}
	return fallbacks(subjectFallback, textFallback)
}

// ntfyPriority decides how loudly the phone announces the message, and the
// emoji tag in front of it:
//   - a new critical problem is "urgent": it is a smoke alarm and has to get
//     through;
//   - a reminder only is "high": the alarm is known already, so it should be
//     less intrusive than the first one;
//   - new warnings are "default";
//   - "resolved" only is "low": good news can wait.
func ntfyPriority(n Notification) (priority, tags string) {
	for _, a := range n.New {
		if a.Severity == checks.Critical {
			return "urgent", "rotating_light"
		}
	}
	switch {
	case len(n.Reminders) > 0:
		return "high", "rotating_light"
	case len(n.New) > 0:
		return "default", "warning"
	default:
		return "low", "white_check_mark"
	}
}
