package config

// minSeverityVariable belongs to one kind of notification target: the
// variable that limits it to critical problems, and whether the target is on.
type minSeverityVariable struct {
	kind    string // as in Notifier.Name and the targets annotation
	env     string
	enabled bool
}

func minSeverityVariables(c Config) []minSeverityVariable {
	n := c.Notify
	return []minSeverityVariable{
		{"mail", "SMOKEALARM_SMTP_MIN_SEVERITY", c.SMTP.Enabled()},
		{"webhook", "SMOKEALARM_WEBHOOK_MIN_SEVERITY", n.WebhookURL != ""},
		{"ntfy", "SMOKEALARM_NTFY_MIN_SEVERITY", n.NtfyTopic != ""},
		{"telegram", "SMOKEALARM_TELEGRAM_MIN_SEVERITY", n.TelegramBotToken != ""},
		{"slack", "SMOKEALARM_SLACK_MIN_SEVERITY", n.SlackWebhookURL != ""},
		{"teams", "SMOKEALARM_TEAMS_MIN_SEVERITY", n.TeamsWebhookURL != ""},
	}
}

// loadCriticalOnly returns the kinds of targets that hear only about critical
// problems. "warning", the default, means everything. Like the other settings
// of a target, the variable is refused if the target is off: probably a typo.
func loadCriticalOnly(r *reader, c Config) []string {
	var kinds []string
	for _, v := range minSeverityVariables(c) {
		value := r.getenv(v.env)
		if value == "" {
			continue
		}
		if !v.enabled {
			r.fail("%s: is set, but the %s target is not configured", v.env, v.kind)
			continue
		}
		if r.choice(v.env, "warning", "warning", "critical") == "critical" {
			kinds = append(kinds, v.kind)
		}
	}
	return kinds
}
