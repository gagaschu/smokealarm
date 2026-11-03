package config

import (
	"net/mail"
	"net/url"
)

// SMTPConfig holds the mail settings. Mail is switched on by setting a host.
type SMTPConfig struct {
	Host     string
	Port     int
	TLS      string // "starttls", "tls" (implicit TLS) or "none"
	Username string
	Password string
	From     string
	To       []string
}

// Enabled reports whether mail notifications are configured.
func (c SMTPConfig) Enabled() bool { return c.Host != "" }

func loadSMTP(r *reader) SMTPConfig {
	return SMTPConfig{
		Host:     r.text("SMOKEALARM_SMTP_HOST", ""),
		Port:     r.integer("SMOKEALARM_SMTP_PORT", 587),
		TLS:      r.choice("SMOKEALARM_SMTP_TLS", "starttls", "starttls", "tls", "none"),
		Username: r.text("SMOKEALARM_SMTP_USERNAME", ""),
		Password: r.text("SMOKEALARM_SMTP_PASSWORD", ""),
		From:     r.text("SMOKEALARM_SMTP_FROM", ""),
		To:       r.list("SMOKEALARM_SMTP_TO"),
	}
}

// validateSMTP checks the mail settings, but only if mail is enabled; the
// SMTP variables are ignored otherwise.
func validateSMTP(r *reader, c SMTPConfig) {
	if !c.Enabled() {
		return
	}
	if c.Port < 1 || c.Port > 65535 {
		r.fail("SMOKEALARM_SMTP_PORT: %d is not a valid port", c.Port)
	}
	if c.Username != "" && c.Password == "" {
		r.fail("SMOKEALARM_SMTP_PASSWORD: required when SMOKEALARM_SMTP_USERNAME is set")
	}

	if c.From == "" {
		r.fail("SMOKEALARM_SMTP_FROM: required when SMOKEALARM_SMTP_HOST is set")
	} else if _, err := mail.ParseAddress(c.From); err != nil {
		r.fail("SMOKEALARM_SMTP_FROM: invalid address %q", c.From)
	}

	if len(c.To) == 0 {
		r.fail("SMOKEALARM_SMTP_TO: required when SMOKEALARM_SMTP_HOST is set")
	}
	for _, to := range c.To {
		if _, err := mail.ParseAddress(to); err != nil {
			r.fail("SMOKEALARM_SMTP_TO: invalid address %q", to)
		}
	}
}

// validateBaseURL accepts an empty value (no link in mails) or an http(s) URL.
func validateBaseURL(r *reader, value string) {
	if value == "" {
		return
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.fail("SMOKEALARM_BASE_URL: %q is not an http(s) URL", value)
	}
}
