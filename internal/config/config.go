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

// Config holds all settings. Later phases add database, mail and login
// settings.
type Config struct {
	CheckInterval    time.Duration
	ListenAddr       string
	LogFormat        string // "json", "text" or "auto"
	LogLevel         string // "debug", "info", "warn" or "error"
	IgnoreNamespaces []string
	DisabledChecks   []string

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
		ListenAddr:       r.text("SMOKEALARM_LISTEN_ADDR", ":8080"),
		LogFormat:        r.choice("SMOKEALARM_LOG_FORMAT", "auto", "json", "text", "auto"),
		LogLevel:         r.choice("SMOKEALARM_LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		IgnoreNamespaces: r.list("SMOKEALARM_IGNORE_NAMESPACES"),
		DisabledChecks:   r.list("SMOKEALARM_DISABLED_CHECKS"),
		RestartThreshold: r.integer("SMOKEALARM_RESTART_THRESHOLD", 5),
		PodPendingAfter:  r.duration("SMOKEALARM_POD_PENDING_AFTER", 5*time.Minute),
		PVCPendingAfter:  r.duration("SMOKEALARM_PVC_PENDING_AFTER", 5*time.Minute),
		CronJobGrace:     r.duration("SMOKEALARM_CRONJOB_GRACE", 5*time.Minute),
	}

	if cfg.CheckInterval <= 0 {
		r.fail("SMOKEALARM_CHECK_INTERVAL: must be greater than zero")
	}
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
