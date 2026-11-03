package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestNoTargetIsOnByDefault(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := NotifyConfig{NtfyServer: "https://ntfy.sh"}
	if !reflect.DeepEqual(cfg.Notify, want) {
		t.Errorf("Notify = %+v, want %+v", cfg.Notify, want)
	}
}

func TestAllTargets(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SMOKEALARM_WEBHOOK_URL":        "https://relay.example.org/hook",
		"SMOKEALARM_WEBHOOK_HEADER":     "Authorization: Bearer abc:def",
		"SMOKEALARM_NTFY_URL":           "https://ntfy.example.org",
		"SMOKEALARM_NTFY_TOPIC":         "smokealarm_x7k2-a",
		"SMOKEALARM_NTFY_TOKEN":         "tk_123",
		"SMOKEALARM_TELEGRAM_BOT_TOKEN": "123456:AAH-xyz_9",
		"SMOKEALARM_TELEGRAM_CHAT_IDS":  "-1001234567890, 42, @my_channel",
		"SMOKEALARM_SLACK_WEBHOOK_URL":  "https://hooks.slack.com/services/T0/B0/X",
		"SMOKEALARM_TEAMS_WEBHOOK_URL":  "https://prod-01.westeurope.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?sig=x",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := NotifyConfig{
		WebhookURL:         "https://relay.example.org/hook",
		WebhookHeaderName:  "Authorization",
		WebhookHeaderValue: "Bearer abc:def",
		NtfyServer:         "https://ntfy.example.org",
		NtfyTopic:          "smokealarm_x7k2-a",
		NtfyToken:          "tk_123",
		TelegramBotToken:   "123456:AAH-xyz_9",
		TelegramChatIDs:    []string{"-1001234567890", "42", "@my_channel"},
		SlackWebhookURL:    "https://hooks.slack.com/services/T0/B0/X",
		TeamsWebhookURL:    "https://prod-01.westeurope.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?sig=x",
	}
	if !reflect.DeepEqual(cfg.Notify, want) {
		t.Errorf("Notify =\n%+v\nwant\n%+v", cfg.Notify, want)
	}
}

func TestTargetValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // empty: valid
	}{
		{"webhook over http", map[string]string{"SMOKEALARM_WEBHOOK_URL": "http://relay.example.org/hook"},
			"SMOKEALARM_WEBHOOK_URL: must use https (plain http only for localhost, or with SMOKEALARM_NOTIFY_ALLOW_HTTP=true)"},
		{"webhook over http with opt-in", map[string]string{"SMOKEALARM_WEBHOOK_URL": "http://apprise.monitoring.svc:8000/notify", "SMOKEALARM_NOTIFY_ALLOW_HTTP": "true"}, ""},
		{"webhook to localhost", map[string]string{"SMOKEALARM_WEBHOOK_URL": "http://localhost:9000/"}, ""},
		{"webhook to 127.0.0.1", map[string]string{"SMOKEALARM_WEBHOOK_URL": "http://127.0.0.1:9000/"}, ""},
		{"webhook without scheme", map[string]string{"SMOKEALARM_WEBHOOK_URL": "relay.example.org/hook"}, "SMOKEALARM_WEBHOOK_URL: not a valid https URL"},
		{"webhook ftp", map[string]string{"SMOKEALARM_WEBHOOK_URL": "ftp://relay.example.org/"}, "SMOKEALARM_WEBHOOK_URL: not a valid https URL"},
		{"header without url", map[string]string{"SMOKEALARM_WEBHOOK_HEADER": "Authorization: Bearer x"}, "SMOKEALARM_WEBHOOK_HEADER: is set, but SMOKEALARM_WEBHOOK_URL is not"},
		{"header without colon", map[string]string{"SMOKEALARM_WEBHOOK_URL": "https://r.example.org", "SMOKEALARM_WEBHOOK_HEADER": "Bearer x"}, "must look like \"Name: value\""},
		{"header without value", map[string]string{"SMOKEALARM_WEBHOOK_URL": "https://r.example.org", "SMOKEALARM_WEBHOOK_HEADER": "Authorization:"}, "must look like \"Name: value\""},
		{"header with bad name", map[string]string{"SMOKEALARM_WEBHOOK_URL": "https://r.example.org", "SMOKEALARM_WEBHOOK_HEADER": "Auth orization: x"}, "not a valid header name"},
		{"header on two lines", map[string]string{"SMOKEALARM_WEBHOOK_URL": "https://r.example.org", "SMOKEALARM_WEBHOOK_HEADER": "X-Key: a\nX-Other: b"}, "must be a single line"},
		{"ntfy default server", map[string]string{"SMOKEALARM_NTFY_TOPIC": "alerts"}, ""},
		{"ntfy self-hosted over http", map[string]string{"SMOKEALARM_NTFY_TOPIC": "alerts", "SMOKEALARM_NTFY_URL": "http://ntfy.ntfy.svc"}, "SMOKEALARM_NTFY_URL: must use https"},
		{"ntfy self-hosted with opt-in", map[string]string{"SMOKEALARM_NTFY_TOPIC": "alerts", "SMOKEALARM_NTFY_URL": "http://ntfy.ntfy.svc", "SMOKEALARM_NOTIFY_ALLOW_HTTP": "true"}, ""},
		{"ntfy topic with slash", map[string]string{"SMOKEALARM_NTFY_TOPIC": "a/b"}, "SMOKEALARM_NTFY_TOPIC: may only contain"},
		{"ntfy topic too long", map[string]string{"SMOKEALARM_NTFY_TOPIC": strings.Repeat("a", 65)}, "SMOKEALARM_NTFY_TOPIC: may only contain"},
		{"ntfy token without topic", map[string]string{"SMOKEALARM_NTFY_TOKEN": "tk"}, "SMOKEALARM_NTFY_TOKEN: is set, but SMOKEALARM_NTFY_TOPIC is not"},
		{"ntfy url ignored without topic", map[string]string{"SMOKEALARM_NTFY_URL": "nonsense"}, ""},
		{"telegram without chats", map[string]string{"SMOKEALARM_TELEGRAM_BOT_TOKEN": "1:abc"}, "SMOKEALARM_TELEGRAM_CHAT_IDS: required when SMOKEALARM_TELEGRAM_BOT_TOKEN is set"},
		{"telegram chats without token", map[string]string{"SMOKEALARM_TELEGRAM_CHAT_IDS": "42"}, "SMOKEALARM_TELEGRAM_CHAT_IDS: is set, but SMOKEALARM_TELEGRAM_BOT_TOKEN is not"},
		{"telegram bad token", map[string]string{"SMOKEALARM_TELEGRAM_BOT_TOKEN": "not-a-token", "SMOKEALARM_TELEGRAM_CHAT_IDS": "42"}, "does not look like a bot token"},
		{"telegram bad chat", map[string]string{"SMOKEALARM_TELEGRAM_BOT_TOKEN": "1:abc", "SMOKEALARM_TELEGRAM_CHAT_IDS": "42,my_channel"}, `"my_channel" is neither a numeric chat id nor a @channelname`},
		{"telegram chat twice", map[string]string{"SMOKEALARM_TELEGRAM_BOT_TOKEN": "1:abc", "SMOKEALARM_TELEGRAM_CHAT_IDS": "42,42"}, `"42" is listed twice`},
		{"slack over http", map[string]string{"SMOKEALARM_SLACK_WEBHOOK_URL": "http://hooks.slack.com/services/x", "SMOKEALARM_NOTIFY_ALLOW_HTTP": "true"}, "SMOKEALARM_SLACK_WEBHOOK_URL: must use https"},
		{"teams over http", map[string]string{"SMOKEALARM_TEAMS_WEBHOOK_URL": "http://example.logic.azure.com/x"}, "SMOKEALARM_TEAMS_WEBHOOK_URL: must use https"},
		{"teams legacy connector", map[string]string{"SMOKEALARM_TEAMS_WEBHOOK_URL": "https://contoso.webhook.office.com/webhookb2/abc/IncomingWebhook/def"}, "Office 365 connector URL"},
		{"bad allow-http", map[string]string{"SMOKEALARM_NOTIFY_ALLOW_HTTP": "maybe"}, "SMOKEALARM_NOTIFY_ALLOW_HTTP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The values of these variables are secrets. However wrong they are, they
// must never show up in the error.
func TestTargetErrorsNeverShowSecrets(t *testing.T) {
	secrets := map[string]string{
		"SMOKEALARM_WEBHOOK_URL":        "http://relay.example.org/hook/SECRET-1",
		"SMOKEALARM_WEBHOOK_HEADER":     "X Bad: SECRET-2",
		"SMOKEALARM_NTFY_TOPIC":         "SECRET-3/x",
		"SMOKEALARM_NTFY_URL":           "ftp://SECRET-4",
		"SMOKEALARM_TELEGRAM_BOT_TOKEN": "SECRET-5",
		"SMOKEALARM_TELEGRAM_CHAT_IDS":  "42",
		"SMOKEALARM_SLACK_WEBHOOK_URL":  "http://hooks.slack.com/SECRET-6",
		"SMOKEALARM_TEAMS_WEBHOOK_URL":  "https://x.webhook.office.com/SECRET-7",
	}
	_, err := Load(env(secrets))
	if err == nil {
		t.Fatal("want errors")
	}
	if strings.Count(err.Error(), ";") < 6 {
		t.Errorf("want an error for every variable, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("the error shows a secret: %v", err)
	}
}
