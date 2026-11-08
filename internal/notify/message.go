package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// subjectPrefix marks all messages so they are easy to filter. The built-in
// subject template starts with it too.
const subjectPrefix = "[smokealarm] "

// Subject summarizes the notification in one line, with the built-in
// template (templates/subject.tmpl). A node that is newly down comes first
// ("NODE DOWN: pi-2 (+ 1 resolved)"), because the problems around it are its
// consequences. Otherwise a single change is named ("CRITICAL:
// CrashLoopBackOff media/jellyfin") and several changes are counted ("3 new
// problems, 1 resolved").
func Subject(n Notification) string {
	tmpl, err := builtinTemplate(subjectFile)
	if err == nil {
		var subject string
		if subject, err = renderSubject(tmpl, newTemplateData(n, "")); err == nil {
			return subject
		}
	}
	// Not reached: the built-in templates are tested. A subject must not
	// fail, so it falls back to the counted summary.
	return subjectPrefix + summaryLine(n)
}

// maxNodeNames is how many down nodes the subject names; the rest are
// counted, so the subject stays one line when a whole rack fails.
const maxNodeNames = 3

// summary counts the changes: "2 new problems, 1 resolved". Nodes that are
// newly down are named first: "NODE DOWN: pi-2, pi-3 (+ 1 new problem)".
func summary(n Notification) string {
	nodes, others := splitNodesDown(n.New)
	counted := counts(len(others), len(n.Reminders), len(n.Resolved))
	if len(nodes) == 0 {
		return counted
	}
	var names []string
	for _, a := range nodes {
		if len(names) == maxNodeNames {
			names = append(names, fmt.Sprintf("and %d more", len(nodes)-maxNodeNames))
			break
		}
		names = append(names, a.Object.Name)
	}
	headline := "NODE DOWN: " + strings.Join(names, ", ")
	if counted == "" {
		return headline
	}
	return headline + " (+ " + counted + ")"
}

// summaryLine is summary, or "no changes" for an empty notification.
func summaryLine(n Notification) string {
	if line := summary(n); line != "" {
		return line
	}
	return "no changes"
}

// counts is "2 new problems, 1 still critical, 1 resolved", or "" if all are
// zero.
func counts(newCount, reminders, resolved int) string {
	var parts []string
	switch newCount {
	case 0:
	case 1:
		parts = append(parts, "1 new problem")
	default:
		parts = append(parts, fmt.Sprintf("%d new problems", newCount))
	}
	if reminders > 0 {
		parts = append(parts, fmt.Sprintf("%d still critical", reminders))
	}
	if resolved > 0 {
		parts = append(parts, fmt.Sprintf("%d resolved", resolved))
	}
	return strings.Join(parts, ", ")
}

// splitNodesDown separates the alerts about nodes that are down from the rest,
// keeping the order of each.
func splitNodesDown(alerts []Alert) (nodes, others []Alert) {
	for _, a := range alerts {
		if a.IsNodeDown() {
			nodes = append(nodes, a)
		} else {
			others = append(others, a)
		}
	}
	return nodes, others
}

// Body renders the plaintext mail body with the built-in template
// (templates/mail.tmpl).
func Body(n Notification) (string, error) {
	tmpl, err := builtinTemplate(mailFile)
	if err != nil {
		return "", err
	}
	return render(tmpl, newTemplateData(n, "mail"))
}

// HTMLBody renders the HTML mail body with the built-in template
// (templates/mail.html.tmpl).
func HTMLBody(n Notification) (string, error) {
	tmpl, err := builtinHTMLTemplate()
	if err != nil {
		return "", err
	}
	return render(tmpl, newTemplateData(n, "mail"))
}

// oneLine puts a text on a single line. Part of a message comes from the
// cluster (image pull errors, event texts), and whoever can write there could
// otherwise add lines that look like a section of the mail or a link.
// strings.Fields splits at every kind of whitespace, also at Unicode line
// breaks.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// ChatBody is the short text for chat targets (ntfy, Telegram, Slack, Teams),
// with the built-in template (templates/chat.tmpl): one line per problem with
// object, reason and since when, grouped like the mail, and the link to the
// web UI. The headline (Subject) is not part of it, because most targets show
// it separately.
//
// The result is at most limit bytes long (see fitChat).
func ChatBody(n Notification, limit int) string {
	tmpl, err := builtinTemplate(chatFile)
	if err == nil {
		var text string
		if text, err = fitChat(tmpl, newTemplateData(n, ""), limit); err == nil {
			return text
		}
	}
	// Not reached: the built-in templates are tested.
	return cutText(summaryLine(n), limit)
}

// cutText shortens s to at most limit bytes without splitting a character, and
// marks the cut with "…". It is for single fields with a hard limit, such as
// the Slack header. A limit too small for the "…" gives as much of s as fits,
// and a limit of 0 or less gives nothing.
func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	if limit < len(ellipsis) {
		return s[:runeStart(s, max(limit, 0))]
	}
	return s[:runeStart(s, limit-len(ellipsis))] + ellipsis
}

// runeStart is the largest index of at most end where a character of s
// starts, so that s[:index] does not split one. end must be below len(s).
func runeStart(s string, end int) int {
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return end
}
