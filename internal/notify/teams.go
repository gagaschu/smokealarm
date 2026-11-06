package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Teams limits a whole message, card included, to about 28 KB. Every line of
// the text costs a block of its own in the card, so the limit for the text
// alone says little: teamsLimit is where it starts, and teamsMessage shortens
// the text until the message is below teamsMaxMessage.
const (
	teamsLimit      = 20000
	teamsMaxMessage = 26000
)

// Teams posts the notification to a channel through a Power Automate
// Workflows webhook ("Post to a channel when a webhook request is received"),
// as an Adaptive Card. The old Office 365 connector webhooks were switched off
// by Microsoft and are not supported.
type Teams struct {
	WebhookURL string // a secret: anybody with it can post to the channel
	Client     *http.Client
	// Templates are the user's message templates; nil means the built-in ones.
	Templates *Templates
}

// Name identifies the target.
func (t *Teams) Name() string { return "teams" }

// Description shows nothing of the URL, which is a secret as a whole.
func (t *Teams) Description() string { return "teams" }

// Send posts the card. Workflows answers 202 Accepted; any 2xx counts. A
// *FallbackError means the message went out with a built-in template.
func (t *Teams) Send(ctx context.Context, n Notification) error {
	subject, subjectFallback := t.Templates.subject("teams", n)
	body, textFallback, err := teamsMessage(t.Templates, n, subject)
	if err != nil {
		return fmt.Errorf("teams: encode message: %w", err)
	}
	err = post(ctx, t.Client, request{
		target:      "teams",
		url:         t.WebhookURL,
		contentType: "application/json",
		body:        body,
	})
	if err != nil {
		return err
	}
	return fallbacks(subjectFallback, textFallback)
}

// teamsMessage encodes the message, with as much of the text as fits. A retry
// sends the same size, so a message that is too long would never get through.
func teamsMessage(templates *Templates, n Notification, subject string) ([]byte, *FallbackError, error) {
	for limit := teamsLimit; ; limit = limit * 3 / 4 {
		text, fallback := templates.chatBody("teams", n, limit)
		body, err := json.Marshal(map[string]any{
			"type": "message",
			"attachments": []any{map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"contentUrl":  nil,
				"content":     teamsCard(subject, text, n.BaseURL),
			}},
		})
		if err != nil || len(body) <= teamsMaxMessage || limit < teamsLimit/100 {
			return body, fallback, err
		}
	}
}

// teamsCard is the Adaptive Card: the subject in bold, the text, and a button
// to the web UI if its address is known.
func teamsCard(subject, text, baseURL string) map[string]any {
	body := []any{
		map[string]any{"type": "TextBlock", "text": subject, "weight": "Bolder", "size": "Medium", "wrap": true},
	}
	body = append(body, teamsLines(text)...)
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if baseURL != "" {
		card["actions"] = []any{
			map[string]any{"type": "Action.OpenUrl", "title": "Open smokealarm", "url": baseURL},
		}
	}
	return card
}

// teamsLines puts every line of the text into its own TextBlock. Teams does
// not reliably keep single line breaks inside one block, and a list of blocks
// looks the same in every client. An empty line becomes extra space above the
// next block.
func teamsLines(text string) []any {
	var blocks []any
	spacing := "Small"
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			spacing = "Medium"
			continue
		}
		blocks = append(blocks, map[string]any{"type": "TextBlock", "text": line, "wrap": true, "spacing": spacing})
		spacing = "None"
	}
	return blocks
}
