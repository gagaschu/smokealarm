package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gagaschu/smokealarm/internal/config"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// newNotifiers creates one notifier per configured target. Without any, state
// changes only show up in the log. Telegram gets one notifier per chat, so a
// chat that fails is retried on its own. All HTTP targets share the client.
func newNotifiers(cfg config.Config, logger *slog.Logger, client *http.Client, now func() time.Time) []notify.Notifier {
	var list []notify.Notifier
	if smtp := cfg.SMTP; smtp.Enabled() {
		list = append(list, &notify.Mail{
			Host:     smtp.Host,
			Port:     smtp.Port,
			TLS:      smtp.TLS,
			Username: smtp.Username,
			Password: smtp.Password,
			From:     smtp.From,
			To:       smtp.To,
			Now:      now,
		})
	}
	n := cfg.Notify
	if n.WebhookURL != "" {
		list = append(list, &notify.Webhook{URL: n.WebhookURL, HeaderName: n.WebhookHeaderName, HeaderValue: n.WebhookHeaderValue, Client: client})
	}
	if n.NtfyTopic != "" {
		list = append(list, &notify.Ntfy{Server: n.NtfyServer, Topic: n.NtfyTopic, Token: n.NtfyToken, Client: client})
	}
	if n.TelegramBotToken != "" {
		for _, chat := range n.TelegramChatIDs {
			list = append(list, &notify.Telegram{APIURL: notify.TelegramAPI, Token: n.TelegramBotToken, ChatID: chat, Client: client})
		}
	}
	if n.SlackWebhookURL != "" {
		list = append(list, &notify.Slack{WebhookURL: n.SlackWebhookURL, Client: client})
	}
	if n.TeamsWebhookURL != "" {
		list = append(list, &notify.Teams{WebhookURL: n.TeamsWebhookURL, Client: client})
	}
	if len(list) == 0 {
		list = append(list, &notify.Log{Logger: logger})
	}
	return list
}

// syncTargets tells the store which targets are configured, so a new one
// starts from the current state, and logs them without secrets.
func syncTargets(ctx context.Context, logger *slog.Logger, db *store.Store, notifiers []notify.Notifier) error {
	var names []string
	for _, n := range notifiers {
		names = append(names, n.Name())
		logger.Info("notification target", "target", n.Description())
	}
	added, removed, err := db.SyncTargets(ctx, names, time.Now())
	if err != nil {
		return fmt.Errorf("register notification targets: %w", err)
	}
	if len(added) > 0 || len(removed) > 0 {
		logger.Info("notification targets changed; new ones start from the current state", "added", added, "removed", removed)
	}
	return nil
}
