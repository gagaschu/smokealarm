package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Slack limits: a header block holds 150 characters, a section 3000.
const (
	slackHeaderLimit  = 150
	slackSectionLimit = 3000
)

// Slack posts the notification to a channel through an incoming webhook.
type Slack struct {
	WebhookURL string // a secret: anybody with it can post to the channel
	Client     *http.Client
}

// Name identifies the target.
func (s *Slack) Name() string { return "slack" }

// Description shows nothing of the URL, which is a secret as a whole.
func (s *Slack) Description() string { return "slack" }

// Send posts the subject as a header and the short text below it. "text" is
// what Slack shows in notifications on the phone.
func (s *Slack) Send(ctx context.Context, n Notification) error {
	subject := Subject(n)
	body, err := json.Marshal(map[string]any{
		"text": subject,
		"blocks": []any{
			map[string]any{
				"type": "header",
				"text": map[string]any{"type": "plain_text", "text": cutText(subject, slackHeaderLimit)},
			},
			map[string]any{
				"type": "section",
				"text": map[string]any{"type": "plain_text", "text": ChatBody(n, slackSectionLimit)},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("slack: encode message: %w", err)
	}
	return post(ctx, s.Client, request{
		target:      "slack",
		url:         s.WebhookURL,
		contentType: "application/json",
		body:        body,
	})
}
