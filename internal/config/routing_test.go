package config

import (
	"slices"
	"strings"
	"testing"
)

func TestCriticalOnly(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    []string
		wantErr string // empty: valid
	}{
		{"default: everything everywhere", map[string]string{"SMOKEALARM_NTFY_TOPIC": "alarms"}, nil, ""},
		{"ntfy critical, slack everything", map[string]string{
			"SMOKEALARM_NTFY_TOPIC":         "alarms",
			"SMOKEALARM_NTFY_MIN_SEVERITY":  "critical",
			"SMOKEALARM_SLACK_WEBHOOK_URL":  "https://hooks.slack.com/services/T0/B0/X",
			"SMOKEALARM_SLACK_MIN_SEVERITY": "warning",
		}, []string{"ntfy"}, ""},
		{"mail and telegram", map[string]string{
			"SMOKEALARM_SMTP_HOST":             "smtp.example.com",
			"SMOKEALARM_SMTP_FROM":             "smokealarm@example.com",
			"SMOKEALARM_SMTP_TO":               "ops@example.com",
			"SMOKEALARM_SMTP_MIN_SEVERITY":     "critical",
			"SMOKEALARM_TELEGRAM_BOT_TOKEN":    "123456:AAH-xyz_9",
			"SMOKEALARM_TELEGRAM_CHAT_IDS":     "42",
			"SMOKEALARM_TELEGRAM_MIN_SEVERITY": "critical",
		}, []string{"mail", "telegram"}, ""},
		{"invalid value", map[string]string{"SMOKEALARM_NTFY_TOPIC": "alarms", "SMOKEALARM_NTFY_MIN_SEVERITY": "high"}, nil,
			`SMOKEALARM_NTFY_MIN_SEVERITY: "high" is not one of warning, critical`},
		{"target is off", map[string]string{"SMOKEALARM_TEAMS_MIN_SEVERITY": "critical"}, nil,
			"SMOKEALARM_TEAMS_MIN_SEVERITY: is set, but the teams target is not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !slices.Equal(cfg.CriticalOnly, tt.want) {
				t.Errorf("CriticalOnly = %v, want %v", cfg.CriticalOnly, tt.want)
			}
		})
	}
}
