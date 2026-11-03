package config

import (
	"reflect"
	"strings"
	"testing"
)

func validMailEnv() map[string]string {
	return map[string]string{
		"SMOKEALARM_SMTP_HOST": "mail.example.com",
		"SMOKEALARM_SMTP_FROM": "smokealarm@example.com",
		"SMOKEALARM_SMTP_TO":   "me@example.com",
	}
}

func TestMailIsOffByDefault(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTP.Enabled() {
		t.Error("mail must be off without SMOKEALARM_SMTP_HOST")
	}
}

func TestMailRulesApplyOnlyWhenEnabled(t *testing.T) {
	// Sender and recipients are only validated when a host is set. Syntax
	// errors in single values (like the port number) are always reported.
	_, err := Load(env(map[string]string{"SMOKEALARM_SMTP_PORT": "abc", "SMOKEALARM_SMTP_FROM": "nonsense"}))
	if err == nil || !strings.Contains(err.Error(), "SMOKEALARM_SMTP_PORT") {
		t.Fatalf("got %v; a malformed port number must always be reported", err)
	}
	if strings.Contains(err.Error(), "SMOKEALARM_SMTP_FROM") {
		t.Errorf("got %v; the sender must not be validated when mail is off", err)
	}
}

func TestMailDefaults(t *testing.T) {
	cfg, err := Load(env(validMailEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SMTP.Enabled() || cfg.SMTP.Port != 587 || cfg.SMTP.TLS != "starttls" {
		t.Errorf("unexpected defaults: %+v", cfg.SMTP)
	}
}

func TestMailFullConfig(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SMOKEALARM_BASE_URL":      "https://smokealarm.example.com",
		"SMOKEALARM_SMTP_HOST":     "mail.example.com",
		"SMOKEALARM_SMTP_PORT":     "465",
		"SMOKEALARM_SMTP_TLS":      "tls",
		"SMOKEALARM_SMTP_USERNAME": "user",
		"SMOKEALARM_SMTP_PASSWORD": "secret",
		"SMOKEALARM_SMTP_FROM":     "smokealarm@example.com",
		"SMOKEALARM_SMTP_TO":       "me@example.com, \"Ops\" <ops@example.com>",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := SMTPConfig{
		Host: "mail.example.com", Port: 465, TLS: "tls", Username: "user", Password: "secret",
		From: "smokealarm@example.com", To: []string{"me@example.com", "\"Ops\" <ops@example.com>"},
	}
	if !reflect.DeepEqual(cfg.SMTP, want) {
		t.Errorf("got %+v, want %+v", cfg.SMTP, want)
	}
	if cfg.BaseURL != "https://smokealarm.example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestMailValidation(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]string // applied on top of a valid config; "" removes the variable
		want   string
	}{
		{"missing from", map[string]string{"SMOKEALARM_SMTP_FROM": ""}, "SMOKEALARM_SMTP_FROM: required"},
		{"invalid from", map[string]string{"SMOKEALARM_SMTP_FROM": "not an address"}, "SMOKEALARM_SMTP_FROM: invalid"},
		{"missing to", map[string]string{"SMOKEALARM_SMTP_TO": ""}, "SMOKEALARM_SMTP_TO: required"},
		{"invalid to", map[string]string{"SMOKEALARM_SMTP_TO": "me@example.com,broken"}, `invalid address "broken"`},
		{"port too high", map[string]string{"SMOKEALARM_SMTP_PORT": "70000"}, "SMOKEALARM_SMTP_PORT"},
		{"port zero", map[string]string{"SMOKEALARM_SMTP_PORT": "0"}, "SMOKEALARM_SMTP_PORT"},
		{"unknown tls mode", map[string]string{"SMOKEALARM_SMTP_TLS": "ssl"}, "SMOKEALARM_SMTP_TLS"},
		{"username without password", map[string]string{"SMOKEALARM_SMTP_USERNAME": "user"}, "SMOKEALARM_SMTP_PASSWORD"},
		{"base url without scheme", map[string]string{"SMOKEALARM_BASE_URL": "smokealarm.example.com"}, "SMOKEALARM_BASE_URL"},
		{"base url with other scheme", map[string]string{"SMOKEALARM_BASE_URL": "ftp://example.com"}, "SMOKEALARM_BASE_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validMailEnv()
			for k, v := range tt.change {
				values[k] = v
			}
			_, err := Load(env(values))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestPasswordIsNeverPartOfAnError(t *testing.T) {
	values := validMailEnv()
	values["SMOKEALARM_SMTP_PASSWORD"] = "hunter2"
	values["SMOKEALARM_SMTP_PORT"] = "abc"
	_, err := Load(env(values))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
	}
}
