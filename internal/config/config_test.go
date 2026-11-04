package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// testHash is structurally a valid bcrypt hash; the tests never check a
// password against it.
const testHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5BAmGZ2XJ0n6ZbQlm9y0rKQ2sXG7y"

// adminEnv holds the variables that have no default.
func adminEnv() map[string]string {
	return map[string]string{
		"SMOKEALARM_ADMIN_USER":          "admin",
		"SMOKEALARM_ADMIN_PASSWORD_HASH": testHash,
	}
}

// env returns a lookup function over the given values plus the required
// admin variables (which the given values may override).
func env(values map[string]string) func(string) string {
	all := adminEnv()
	for k, v := range values {
		all[k] = v
	}
	return func(name string) string { return all[name] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		CheckInterval:    30 * time.Second,
		AlertAfter:       2 * time.Minute,
		Retention:        30 * 24 * time.Hour,
		DBPath:           "smokealarm.db",
		ListenAddr:       ":8080",
		LogFormat:        "auto",
		LogLevel:         "info",
		RestartThreshold: 5,
		PodPendingAfter:  5 * time.Minute,
		PVCPendingAfter:  5 * time.Minute,
		CronJobGrace:     5 * time.Minute,
		SMTP:             SMTPConfig{Port: 587, TLS: "starttls"},
		Notify:           NotifyConfig{NtfyServer: "https://ntfy.sh"},

		AdminUser:         "admin",
		AdminPasswordHash: testHash,
		SessionTTL:        7 * 24 * time.Hour,
		CookieSecure:      true,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SMOKEALARM_CHECK_INTERVAL":           "1m",
		"SMOKEALARM_ALERT_AFTER":              "0s",
		"SMOKEALARM_CRITICAL_REPEAT_INTERVAL": "4h",
		"SMOKEALARM_RETENTION":                "7d",
		"SMOKEALARM_DB_PATH":                  "/data/smokealarm.db",
		"SMOKEALARM_LISTEN_ADDR":              "127.0.0.1:9000",
		"SMOKEALARM_LOG_FORMAT":               "text",
		"SMOKEALARM_LOG_LEVEL":                "debug",
		"SMOKEALARM_IGNORE_NAMESPACES":        "kube-system, test ,,",
		"SMOKEALARM_RESTART_THRESHOLD":        "10",
		"SMOKEALARM_POD_PENDING_AFTER":        "2d",
		"SMOKEALARM_PVC_PENDING_AFTER":        "10m",
		"SMOKEALARM_CRONJOB_GRACE":            "90s",
		"SMOKEALARM_DISABLED_CHECKS":          "pod-restarts, pvc-pending",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PVCPendingAfter != 10*time.Minute || cfg.CronJobGrace != 90*time.Second {
		t.Errorf("PVCPendingAfter = %v, CronJobGrace = %v", cfg.PVCPendingAfter, cfg.CronJobGrace)
	}
	if want := []string{"pod-restarts", "pvc-pending"}; !reflect.DeepEqual(cfg.DisabledChecks, want) {
		t.Errorf("DisabledChecks = %v, want %v", cfg.DisabledChecks, want)
	}
	if cfg.CheckInterval != time.Minute {
		t.Errorf("CheckInterval = %v", cfg.CheckInterval)
	}
	if cfg.AlertAfter != 0 || cfg.Retention != 7*24*time.Hour || cfg.DBPath != "/data/smokealarm.db" {
		t.Errorf("AlertAfter = %v, Retention = %v, DBPath = %q", cfg.AlertAfter, cfg.Retention, cfg.DBPath)
	}
	if cfg.CriticalRepeatInterval != 4*time.Hour {
		t.Errorf("CriticalRepeatInterval = %v, want 4h", cfg.CriticalRepeatInterval)
	}
	if cfg.PodPendingAfter != 48*time.Hour {
		t.Errorf("PodPendingAfter = %v, want 48h", cfg.PodPendingAfter)
	}
	if want := []string{"kube-system", "test"}; !reflect.DeepEqual(cfg.IgnoreNamespaces, want) {
		t.Errorf("IgnoreNamespaces = %v, want %v", cfg.IgnoreNamespaces, want)
	}
	if cfg.RestartThreshold != 10 || cfg.LogFormat != "text" || cfg.LogLevel != "debug" {
		t.Errorf("unexpected config %+v", cfg)
	}
}

func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SMOKEALARM_CHECK_INTERVAL":    "soon",
		"SMOKEALARM_LOG_LEVEL":         "loud",
		"SMOKEALARM_RESTART_THRESHOLD": "abc",
		"SMOKEALARM_POD_PENDING_AFTER": "xd",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"SMOKEALARM_CHECK_INTERVAL",
		"SMOKEALARM_LOG_LEVEL",
		"SMOKEALARM_RESTART_THRESHOLD",
		"SMOKEALARM_POD_PENDING_AFTER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsNonPositiveInterval(t *testing.T) {
	_, err := Load(env(map[string]string{"SMOKEALARM_CHECK_INTERVAL": "0s"}))
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Errorf("got %v, want a 'greater than zero' error", err)
	}
}

func TestLoadRejectsBadAlertAndRetention(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"negative alert after", map[string]string{"SMOKEALARM_ALERT_AFTER": "-1m"}, "SMOKEALARM_ALERT_AFTER"},
		{"negative repeat interval", map[string]string{"SMOKEALARM_CRITICAL_REPEAT_INTERVAL": "-1h"}, "SMOKEALARM_CRITICAL_REPEAT_INTERVAL"},
		{"repeat interval below check interval", map[string]string{"SMOKEALARM_CRITICAL_REPEAT_INTERVAL": "10s"}, "SMOKEALARM_CRITICAL_REPEAT_INTERVAL"},
		{"zero retention", map[string]string{"SMOKEALARM_RETENTION": "0s"}, "SMOKEALARM_RETENTION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error mentioning %s", err, tt.want)
			}
		})
	}
}

func TestAdminVariablesAreRequired(t *testing.T) {
	// Not through env(): that helper fills in the admin variables.
	_, err := Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"SMOKEALARM_ADMIN_USER is required", "SMOKEALARM_ADMIN_PASSWORD_HASH is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestAdminPasswordHashMustBeBcrypt(t *testing.T) {
	_, err := Load(env(map[string]string{"SMOKEALARM_ADMIN_PASSWORD_HASH": "my-plain-password"}))
	if err == nil || !strings.Contains(err.Error(), "not a valid bcrypt hash") {
		t.Fatalf("got %v, want a bcrypt hash error", err)
	}
	if strings.Contains(err.Error(), "my-plain-password") {
		t.Errorf("error repeats the value: %v", err)
	}
}

func TestSessionAndCookieSettings(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SMOKEALARM_SESSION_TTL":   "12h",
		"SMOKEALARM_COOKIE_SECURE": "false",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionTTL != 12*time.Hour || cfg.CookieSecure {
		t.Errorf("SessionTTL = %v, CookieSecure = %v", cfg.SessionTTL, cfg.CookieSecure)
	}

	for name, values := range map[string]map[string]string{
		"zero ttl":     {"SMOKEALARM_SESSION_TTL": "0s"},
		"bad boolean":  {"SMOKEALARM_COOKIE_SECURE": "maybe"},
		"bad duration": {"SMOKEALARM_SESSION_TTL": "forever"},
	} {
		if _, err := Load(env(values)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
