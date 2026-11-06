package main

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/config"
	"github.com/gagaschu/smokealarm/internal/notify"
)

func names(list []notify.Notifier) string {
	var out []string
	for _, n := range list {
		out = append(out, n.Name())
	}
	return strings.Join(out, ",")
}

func TestWithoutTargetsNotificationsGoToTheLog(t *testing.T) {
	list := newNotifiers(config.Config{}, slog.Default(), notify.NewHTTPClient(), time.Now, nil)
	if names(list) != "log" {
		t.Errorf("targets = %s, want log", names(list))
	}
}

func TestEveryConfiguredTargetGetsANotifier(t *testing.T) {
	cfg := config.Config{
		SMTP: config.SMTPConfig{Host: "mail.example.com"},
		Notify: config.NotifyConfig{
			WebhookURL:       "https://relay.example.org/secret-path",
			NtfyServer:       "https://ntfy.example.org",
			NtfyTopic:        "secret-topic",
			TelegramBotToken: "1:secret-token",
			TelegramChatIDs:  []string{"-100123", "@ops_channel"},
			SlackWebhookURL:  "https://hooks.slack.com/services/secret-slack",
			TeamsWebhookURL:  "https://example.logic.azure.com/secret-teams",
		},
	}
	list := newNotifiers(cfg, slog.Default(), notify.NewHTTPClient(), time.Now, nil)
	if got := names(list); got != "mail,webhook,ntfy,telegram:-100123,telegram:@ops_channel,slack,teams" {
		t.Errorf("targets = %s", got)
	}
	// The descriptions go to the log and the status page.
	for _, n := range list {
		if strings.Contains(n.Description(), "secret") {
			t.Errorf("description %q shows a secret", n.Description())
		}
	}
}

func TestMinSeveritiesCoverEveryTelegramChat(t *testing.T) {
	cfg := config.Config{Notify: config.NotifyConfig{
		NtfyTopic:        "topic",
		TelegramBotToken: "1:token",
		TelegramChatIDs:  []string{"-100123", "42"},
		SlackWebhookURL:  "https://hooks.slack.com/services/x",
	}}
	list := newNotifiers(cfg, slog.Default(), notify.NewHTTPClient(), time.Now, nil)
	got := minSeverities(list, []string{"telegram", "ntfy"})
	want := map[string]checks.Severity{"ntfy": checks.Critical, "telegram:-100123": checks.Critical, "telegram:42": checks.Critical}
	if !maps.Equal(got, want) {
		t.Errorf("minSeverities = %v, want %v", got, want)
	}
}

// Every target that has templates gets them, and the status page learns which
// ones each target uses.
func TestNotifiersGetTheTemplates(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"subject.tmpl": "S", "slack.chat.tmpl": "C", "teams.chat.tmpl": "T"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	templates, err := notify.LoadTemplates(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		SMTP: config.SMTPConfig{Host: "mail.example.com"},
		Notify: config.NotifyConfig{
			WebhookURL:       "https://relay.example.org/x",
			NtfyTopic:        "topic",
			TelegramBotToken: "1:token",
			TelegramChatIDs:  []string{"42"},
			SlackWebhookURL:  "https://hooks.slack.com/services/x",
		},
	}
	list := newNotifiers(cfg, slog.Default(), notify.NewHTTPClient(), time.Now, templates)
	for _, n := range list {
		var got *notify.Templates
		switch target := n.(type) {
		case *notify.Mail:
			got = target.Templates
		case *notify.Ntfy:
			got = target.Templates
		case *notify.Telegram:
			got = target.Templates
		case *notify.Slack:
			got = target.Templates
		case *notify.Teams:
			got = target.Templates
		default:
			continue
		}
		if got != templates {
			t.Errorf("%s does not have the templates", n.Name())
		}
	}

	want := map[string]string{
		"mail": "subject.tmpl", "ntfy": "subject.tmpl", "telegram:42": "subject.tmpl",
		"slack": "subject.tmpl, slack.chat.tmpl",
	}
	if got := templateFiles(list, templates); !maps.Equal(got, want) {
		t.Errorf("templateFiles = %v, want %v", got, want)
	}
	if got := strings.Join(unusedTemplates(list, templates), ","); got != "teams.chat.tmpl" {
		t.Errorf("unusedTemplates = %q, want teams.chat.tmpl (Teams is off)", got)
	}
}
