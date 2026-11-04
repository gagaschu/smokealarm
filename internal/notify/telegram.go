package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// TelegramAPI is the address of the Telegram Bot API, the default for
// Telegram.APIURL.
const TelegramAPI = "https://api.telegram.org"

// telegramLimit is the length limit of one Telegram message (4096 characters).
const telegramLimit = 4096

// Telegram sends the notification to one chat through a bot. main creates one
// per chat id, so a chat that fails is retried without the others getting the
// message twice.
type Telegram struct {
	APIURL string // TelegramAPI; tests point it to a local server
	// Token is the bot token. It is part of the URL path, so the URL never
	// appears in a log or an error.
	Token  string
	ChatID string // a number (-1001234...) or a public channel (@name)
	Client *http.Client
}

// Name identifies the target, one per chat.
func (t *Telegram) Name() string { return "telegram:" + t.ChatID }

// Description shows the chat, which is not a secret.
func (t *Telegram) Description() string { return "telegram (chat " + t.ChatID + ")" }

// Send posts the subject and the short text. It is plain text without
// parse_mode, so names with "_" or "<" need no escaping.
func (t *Telegram) Send(ctx context.Context, n Notification) error {
	subject := Subject(n)
	text := subject + "\n\n" + ChatBody(n, telegramLimit-len(subject)-2)
	body, err := json.Marshal(map[string]any{
		"chat_id":              t.ChatID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	})
	if err != nil {
		return fmt.Errorf("telegram: encode message: %w", err)
	}
	return post(ctx, t.Client, request{
		target:      "telegram",
		url:         strings.TrimRight(t.APIURL, "/") + "/bot" + t.Token + "/sendMessage",
		contentType: "application/json",
		body:        body,
		secrets:     []string{t.Token},
	})
}
