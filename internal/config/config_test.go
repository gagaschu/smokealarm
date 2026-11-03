package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		CheckInterval:    30 * time.Second,
		ListenAddr:       ":8080",
		LogFormat:        "auto",
		LogLevel:         "info",
		RestartThreshold: 5,
		PodPendingAfter:  5 * time.Minute,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SMOKEALARM_CHECK_INTERVAL":    "1m",
		"SMOKEALARM_LISTEN_ADDR":       "127.0.0.1:9000",
		"SMOKEALARM_LOG_FORMAT":        "text",
		"SMOKEALARM_LOG_LEVEL":         "debug",
		"SMOKEALARM_IGNORE_NAMESPACES": "kube-system, test ,,",
		"SMOKEALARM_RESTART_THRESHOLD": "10",
		"SMOKEALARM_POD_PENDING_AFTER": "2d",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CheckInterval != time.Minute {
		t.Errorf("CheckInterval = %v", cfg.CheckInterval)
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
