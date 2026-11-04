package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
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

// minSeverities gives every notifier of a kind in criticalOnly (from
// SMOKEALARM_<TARGET>_MIN_SEVERITY) the minimum severity critical. All Telegram
// chats share the setting.
func minSeverities(notifiers []notify.Notifier, criticalOnly []string) map[string]checks.Severity {
	severities := map[string]checks.Severity{}
	for _, n := range notifiers {
		if slices.Contains(criticalOnly, notify.Kind(n.Name())) {
			severities[n.Name()] = checks.Critical
		}
	}
	return severities
}

// syncTargets tells the store which targets are configured, so a new one does
// not get the history, and logs them without secrets.
func syncTargets(ctx context.Context, logger *slog.Logger, db *store.Store, notifiers []notify.Notifier,
	criticalOnly []string) error {
	var names []string
	for _, n := range notifiers {
		names = append(names, n.Name())
		hears := "everything"
		if slices.Contains(criticalOnly, notify.Kind(n.Name())) {
			hears = "critical problems only"
		}
		logger.Info("notification target", "target", n.Description(), "hears", hears)
	}
	added, removed, err := db.SyncTargets(ctx, names)
	if err != nil {
		return fmt.Errorf("register notification targets: %w", err)
	}
	if len(added) > 0 || len(removed) > 0 {
		logger.Info("notification targets changed; new ones hear about what is firing now, not the history", "added", added, "removed", removed)
	}
	return nil
}
