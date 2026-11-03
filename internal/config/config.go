// Package config reads the SMOKEALARM_* environment variables, applies
// defaults and validates the result. Everything is checked at startup so a
// typo fails loudly instead of silently changing behavior.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds all settings.
type Config struct {
	CheckInterval    time.Duration
	AlertAfter       time.Duration // how long a problem must persist before it fires
	Retention        time.Duration // how long resolved findings are kept
	DBPath           string
	ListenAddr       string
	LogFormat        string // "json", "text" or "auto"
	LogLevel         string // "debug", "info", "warn" or "error"
	IgnoreNamespaces []string
	DisabledChecks   []string
	BaseURL          string // public URL of the web UI, linked in mails

	SMTP SMTPConfig

	AdminUser         string
	AdminPasswordHash string // bcrypt hash, see "smokealarm hash-password"
	SessionTTL        time.Duration
	CookieSecure      bool // only turn off for local development over plain HTTP

	RestartThreshold int
	PodPendingAfter  time.Duration
	PVCPendingAfter  time.Duration
	CronJobGrace     time.Duration
}

// Load builds the configuration. The lookup function is injected (instead of
// calling os.Getenv) so tests do not depend on the real environment.
// All problems are collected and returned as one error.
func Load(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}

	cfg := Config{
		CheckInterval:    r.duration("SMOKEALARM_CHECK_INTERVAL", 30*time.Second),
		AlertAfter:       r.duration("SMOKEALARM_ALERT_AFTER", 2*time.Minute),
		Retention:        r.duration("SMOKEALARM_RETENTION", 30*24*time.Hour),
		DBPath:           r.text("SMOKEALARM_DB_PATH", "smokealarm.db"),
		ListenAddr:       r.text("SMOKEALARM_LISTEN_ADDR", ":8080"),
		LogFormat:        r.choice("SMOKEALARM_LOG_FORMAT", "auto", "json", "text", "auto"),
		LogLevel:         r.choice("SMOKEALARM_LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		IgnoreNamespaces: r.list("SMOKEALARM_IGNORE_NAMESPACES"),
		DisabledChecks:   r.list("SMOKEALARM_DISABLED_CHECKS"),
		BaseURL:          r.text("SMOKEALARM_BASE_URL", ""),
		SMTP:             loadSMTP(&r),

		AdminUser:         r.required("SMOKEALARM_ADMIN_USER"),
		AdminPasswordHash: r.required("SMOKEALARM_ADMIN_PASSWORD_HASH"),
		SessionTTL:        r.duration("SMOKEALARM_SESSION_TTL", 7*24*time.Hour),
		CookieSecure:      r.boolean("SMOKEALARM_COOKIE_SECURE", true),
		RestartThreshold:  r.integer("SMOKEALARM_RESTART_THRESHOLD", 5),
		PodPendingAfter:   r.duration("SMOKEALARM_POD_PENDING_AFTER", 5*time.Minute),
		PVCPendingAfter:   r.duration("SMOKEALARM_PVC_PENDING_AFTER", 5*time.Minute),
		CronJobGrace:      r.duration("SMOKEALARM_CRONJOB_GRACE", 5*time.Minute),
	}

	if cfg.CheckInterval <= 0 {
		r.fail("SMOKEALARM_CHECK_INTERVAL: must be greater than zero")
	}
	if cfg.AlertAfter < 0 {
		r.fail("SMOKEALARM_ALERT_AFTER: must not be negative")
	}
	if cfg.Retention <= 0 {
		r.fail("SMOKEALARM_RETENTION: must be greater than zero")
	}
	if cfg.SessionTTL <= 0 {
		r.fail("SMOKEALARM_SESSION_TTL: must be greater than zero")
	}
	validatePasswordHash(&r, cfg.AdminPasswordHash)
	validateBaseURL(&r, cfg.BaseURL)
	validateSMTP(&r, cfg.SMTP)
	if cfg.RestartThreshold < 0 {
		r.fail("SMOKEALARM_RESTART_THRESHOLD: must not be negative")
	}

	if len(r.problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(r.problems, "; "))
	}
	return cfg, nil
}

// reader parses single variables and remembers every problem it finds.
type reader struct {
	getenv   func(string) string
	problems []string
}

func (r *reader) fail(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *reader) text(name, def string) string {
	if v := r.getenv(name); v != "" {
		return v
	}
	return def
}

// required reads a variable that has no default. A missing value is reported
// together with all other problems.
func (r *reader) required(name string) string {
	v := r.getenv(name)
	if v == "" {
		r.fail("%s is required", name)
	}
	return v
}

func (r *reader) boolean(name string, def bool) bool {
	v := r.getenv(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail("%s: %q is not true or false", name, v)
		return def
	}
	return b
}

func (r *reader) choice(name, def string, allowed ...string) string {
	v := r.text(name, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	r.fail("%s: %q is not one of %s", name, v, strings.Join(allowed, ", "))
	return def
}

func (r *reader) integer(name string, def int) int {
	v := r.getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail("%s: invalid number %q", name, v)
		return def
	}
	return n
}

func (r *reader) duration(name string, def time.Duration) time.Duration {
	v := r.getenv(name)
	if v == "" {
		return def
	}
	d, err := parseDuration(v)
	if err != nil {
		r.fail("%s: %v", name, err)
		return def
	}
	return d
}

func (r *reader) list(name string) []string {
	return parseList(r.getenv(name))
}

// parseDuration accepts Go durations ("30s", "5m") plus a "d" suffix for
// whole days ("30d"), because time.ParseDuration has no day unit.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// parseList splits a comma-separated list and drops empty entries.
func parseList(s string) []string {
	var items []string
	for _, part := range strings.Split(s, ",") {
		if item := strings.TrimSpace(part); item != "" {
			items = append(items, item)
		}
	}
	return items
}
