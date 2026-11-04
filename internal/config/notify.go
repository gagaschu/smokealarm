package config

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// NotifyConfig holds the notification targets besides mail. Each target is
// switched on by its main variable (URL, topic or token), like mail by
// SMOKEALARM_SMTP_HOST. Several can be active at once.
//
// Many of these values are secrets: tokens, webhook URLs (whoever has one can
// post), the header value, and the ntfy topic (on a public server, whoever
// knows it can read along). Error messages therefore name the variable, never
// its value.
type NotifyConfig struct {
	// AllowHTTP permits plain http:// for the webhook and the ntfy server,
	// for a relay or ntfy inside the cluster. localhost is always allowed.
	AllowHTTP bool

	WebhookURL         string
	WebhookHeaderName  string // from SMOKEALARM_WEBHOOK_HEADER, "Name: value"
	WebhookHeaderValue string

	NtfyServer string
	NtfyTopic  string
	NtfyToken  string

	TelegramBotToken string
	TelegramChatIDs  []string

	SlackWebhookURL string
	TeamsWebhookURL string
}

// defaultNtfyServer is the public ntfy server.
const defaultNtfyServer = "https://ntfy.sh"

// The variables of the notification targets. They appear in several error
// messages, so they are named once here.
const (
	envWebhookURL      = "SMOKEALARM_WEBHOOK_URL"
	envWebhookHeader   = "SMOKEALARM_WEBHOOK_HEADER"
	envNtfyURL         = "SMOKEALARM_NTFY_URL"
	envNtfyTopic       = "SMOKEALARM_NTFY_TOPIC"
	envNtfyToken       = "SMOKEALARM_NTFY_TOKEN"
	envTelegramToken   = "SMOKEALARM_TELEGRAM_BOT_TOKEN"
	envTelegramChatIDs = "SMOKEALARM_TELEGRAM_CHAT_IDS"
	envSlackURL        = "SMOKEALARM_SLACK_WEBHOOK_URL"
	envTeamsURL        = "SMOKEALARM_TEAMS_WEBHOOK_URL"
	envAllowHTTP       = "SMOKEALARM_NOTIFY_ALLOW_HTTP"
)

func loadNotify(r *reader) NotifyConfig {
	c := NotifyConfig{
		AllowHTTP:        r.boolean(envAllowHTTP, false),
		WebhookURL:       r.text(envWebhookURL, ""),
		NtfyServer:       r.text(envNtfyURL, defaultNtfyServer),
		NtfyTopic:        r.text(envNtfyTopic, ""),
		NtfyToken:        r.text(envNtfyToken, ""),
		TelegramBotToken: r.text(envTelegramToken, ""),
		TelegramChatIDs:  r.list(envTelegramChatIDs),
		SlackWebhookURL:  r.text(envSlackURL, ""),
		TeamsWebhookURL:  r.text(envTeamsURL, ""),
	}
	c.WebhookHeaderName, c.WebhookHeaderValue = parseHeader(r, r.text(envWebhookHeader, ""))
	return c
}

// parseHeader splits "Name: value". The value is a secret and is never part
// of an error.
func parseHeader(r *reader, header string) (name, value string) {
	if header == "" {
		return "", ""
	}
	name, value, found := strings.Cut(header, ":")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	headerName := regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")
	switch {
	case !found || value == "":
		r.fail("%s: must look like \"Name: value\", e.g. \"Authorization: Bearer <token>\"", envWebhookHeader)
	case !headerName.MatchString(name):
		r.fail("%s: the part before \":\" is not a valid header name", envWebhookHeader)
	case strings.ContainsAny(value, "\r\n"):
		r.fail("%s: must be a single line", envWebhookHeader)
	default:
		return name, value
	}
	return "", ""
}

// validateNotify checks the targets that are switched on, and complains about
// settings that belong to a target that is off (usually a typo).
func validateNotify(r *reader, c NotifyConfig) {
	validateWebhook(r, c)
	validateNtfy(r, c)
	validateTelegram(r, c)
	if c.SlackWebhookURL != "" {
		checkTargetURL(r, envSlackURL, c.SlackWebhookURL, false)
	}
	if c.TeamsWebhookURL != "" {
		checkTargetURL(r, envTeamsURL, c.TeamsWebhookURL, false)
		if isLegacyTeamsURL(c.TeamsWebhookURL) {
			r.fail("%s: this is an Office 365 connector URL; Microsoft switched those off, create a Workflows webhook instead (see the README)", envTeamsURL)
		}
	}
}

func validateWebhook(r *reader, c NotifyConfig) {
	if c.WebhookURL == "" {
		if c.WebhookHeaderName != "" {
			r.fail("%s: is set, but %s is not", envWebhookHeader, envWebhookURL)
		}
		return
	}
	checkTargetURL(r, envWebhookURL, c.WebhookURL, c.AllowHTTP)
}

func validateNtfy(r *reader, c NotifyConfig) {
	if c.NtfyTopic == "" {
		if c.NtfyToken != "" {
			r.fail("%s: is set, but %s is not", envNtfyToken, envNtfyTopic)
		}
		return
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(c.NtfyTopic) {
		r.fail("%s: may only contain letters, digits, \"-\" and \"_\" (at most 64)", envNtfyTopic)
	}
	// It goes into a header; otherwise every send would fail, not the start.
	if c.NtfyToken != "" && !regexp.MustCompile(`^[\x21-\x7e]+$`).MatchString(c.NtfyToken) {
		r.fail("%s: must be one word of plain characters", envNtfyToken)
	}
	checkTargetURL(r, envNtfyURL, c.NtfyServer, c.AllowHTTP)
}

func validateTelegram(r *reader, c NotifyConfig) {
	if c.TelegramBotToken == "" {
		if len(c.TelegramChatIDs) > 0 {
			r.fail("%s: is set, but %s is not", envTelegramChatIDs, envTelegramToken)
		}
		return
	}
	if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(c.TelegramBotToken) {
		r.fail("%s: does not look like a bot token from @BotFather (<digits>:<characters>)", envTelegramToken)
	}
	if len(c.TelegramChatIDs) == 0 {
		r.fail("%s: required when %s is set", envTelegramChatIDs, envTelegramToken)
	}
	// Chat ids are not secret, so the wrong one may be shown.
	chatID := regexp.MustCompile(`^(-?[0-9]+|@[A-Za-z][A-Za-z0-9_]{4,31})$`)
	for i, id := range c.TelegramChatIDs {
		if !chatID.MatchString(id) {
			r.fail("%s: %q is neither a numeric chat id nor a @channelname", envTelegramChatIDs, id)
		}
		if slices.Contains(c.TelegramChatIDs[:i], id) {
			r.fail("%s: %q is listed twice", envTelegramChatIDs, id)
		}
	}
}

// checkTargetURL requires an https URL with a host. Plain http is accepted for
// localhost, and elsewhere only if allowHTTP is true. The value is never shown,
// because it may be a secret.
func checkTargetURL(r *reader, name, value string, allowHTTP bool) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		r.fail("%s: not a valid https URL", name)
		return
	}
	if u.Scheme == "https" || isLocalhost(u.Hostname()) {
		return
	}
	if allowHTTP {
		return
	}
	if name == envSlackURL || name == envTeamsURL {
		r.fail("%s: must use https", name)
		return
	}
	r.fail("%s: must use https (plain http only for localhost, or with %s=true)", name, envAllowHTTP)
}

func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// isLegacyTeamsURL recognizes the old Office 365 connector webhooks
// (https://<tenant>.webhook.office.com/webhookb2/...), which no longer work.
func isLegacyTeamsURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".webhook.office.com") || host == "outlook.office.com"
}
