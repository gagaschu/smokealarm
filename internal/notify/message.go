package notify

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// subjectPrefix marks all mails so they are easy to filter.
const subjectPrefix = "[smokealarm] "

// timeFormat is used for all times in the mail body. UTC avoids guessing the
// reader's time zone.
const timeFormat = "2006-01-02 15:04:05 UTC"

// Subject summarizes the notification in one line. A single change is named
// ("CRITICAL: CrashLoopBackOff media/jellyfin"); several changes are counted
// ("3 new problems, 1 resolved").
func Subject(n Notification) string {
	if len(n.New)+len(n.Reminders)+len(n.Resolved) != 1 {
		return subjectPrefix + summary(n)
	}
	switch {
	case len(n.New) == 1:
		a := n.New[0]
		return fmt.Sprintf("%s%s: %s %s", subjectPrefix, strings.ToUpper(string(a.Severity)), a.Reason, objectName(a.Object))
	case len(n.Reminders) == 1:
		a := n.Reminders[0]
		return fmt.Sprintf("%sSTILL CRITICAL: %s %s", subjectPrefix, a.Reason, objectName(a.Object))
	default:
		a := n.Resolved[0]
		return fmt.Sprintf("%sRESOLVED: %s %s", subjectPrefix, a.Reason, objectName(a.Object))
	}
}

// summary counts the changes: "2 new problems, 1 resolved".
func summary(n Notification) string {
	var parts []string
	switch len(n.New) {
	case 0:
	case 1:
		parts = append(parts, "1 new problem")
	default:
		parts = append(parts, fmt.Sprintf("%d new problems", len(n.New)))
	}
	if len(n.Reminders) > 0 {
		parts = append(parts, fmt.Sprintf("%d still critical", len(n.Reminders)))
	}
	if len(n.Resolved) > 0 {
		parts = append(parts, fmt.Sprintf("%d resolved", len(n.Resolved)))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// objectName is "namespace/name", or just "name" for cluster-scoped objects
// such as nodes.
func objectName(ref checks.ObjectRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "/" + ref.Name
}

// bodyTemplate is the plaintext mail body. New problems come first, then
// reminders, then resolved ones; the engine has already sorted each list.
const bodyTemplate = `{{.Summary}}
{{if .New}}
NEW PROBLEMS
{{range .New}}
[{{.Severity}}] {{.Reason}}
  Object:  {{.Object}}
  Message: {{.Message}}
  Since:   {{.Since}}
{{end}}{{end}}{{if .Reminders}}
STILL CRITICAL
{{range .Reminders}}
[{{.Severity}}] {{.Reason}}
  Object:  {{.Object}}
  Message: {{.Message}}
  Since:   {{.Since}}
{{end}}{{end}}{{if .Resolved}}
RESOLVED
{{range .Resolved}}
[RESOLVED] {{.Reason}}
  Object:   {{.Object}}
  Message:  {{.Message}}
  Since:    {{.Since}}
  Resolved: {{.ResolvedAt}}
{{end}}{{end}}{{if .BaseURL}}
Details: {{.BaseURL}}
{{end}}`

// bodyData and bodyEntry hold the already formatted values, so the template
// stays free of logic.
type bodyData struct {
	Summary   string
	New       []bodyEntry
	Reminders []bodyEntry
	Resolved  []bodyEntry
	BaseURL   string
}

type bodyEntry struct {
	Severity   string
	Reason     string
	Object     string
	Message    string
	Since      string
	ResolvedAt string
}

// Body renders the plaintext mail body.
func Body(n Notification) (string, error) {
	tmpl, err := template.New("body").Parse(bodyTemplate)
	if err != nil {
		return "", fmt.Errorf("parse body template: %w", err)
	}
	data := bodyData{
		Summary:   summary(n),
		New:       entries(n.New),
		Reminders: entries(n.Reminders),
		Resolved:  entries(n.Resolved),
		BaseURL:   n.BaseURL,
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render body: %w", err)
	}
	return out.String(), nil
}

func entries(alerts []Alert) []bodyEntry {
	var result []bodyEntry
	for _, a := range alerts {
		result = append(result, bodyEntry{
			Severity:   strings.ToUpper(string(a.Severity)),
			Reason:     a.Reason,
			Object:     a.Object.Label(),
			Message:    a.Message,
			Since:      formatTime(a.Since),
			ResolvedAt: formatTime(a.ResolvedAt),
		})
	}
	return result
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// chatTimeFormat is shorter than timeFormat, because a chat line has little
// room.
const chatTimeFormat = "2006-01-02 15:04 UTC"

// chatLine is one line of a chat message: a section heading or one problem.
type chatLine struct {
	text    string
	problem bool
}

// ChatBody is the short text for chat targets (ntfy, Telegram, Slack, Teams):
// one line per problem with object, reason and since when, grouped like the
// mail, and the link to the web UI. The headline (Subject) is not part of it,
// because most targets show it separately.
//
// The result is at most limit bytes long. Bytes, not characters, because that
// is exact for ntfy and on the safe side for the services that count
// characters. If not everything fits, the last line says how many problems are
// left out and where to see them.
func ChatBody(n Notification, limit int) string {
	lines := chatLines(n)
	full := joinChatLines(lines)
	if n.BaseURL != "" {
		full += "\n\n" + n.BaseURL
	}
	if len(full) <= limit {
		return full
	}

	var kept []chatLine
	size := 0
	for i, line := range lines {
		more := moreLine(countProblems(lines[i:]), n.BaseURL)
		if size+len(line.text)+1+len(more) > limit {
			// A heading without any of its problems is only noise.
			if len(kept) > 0 && !kept[len(kept)-1].problem {
				kept = kept[:len(kept)-1]
			}
			if len(kept) == 0 {
				return more
			}
			return joinChatLines(kept) + "\n" + more
		}
		kept = append(kept, line)
		size += len(line.text) + 1
	}
	return joinChatLines(kept) // not reached: the full text did not fit
}

func chatLines(n Notification) []chatLine {
	var lines []chatLine
	add := func(heading string, alerts []Alert, format func(Alert) string) {
		if len(alerts) == 0 {
			return
		}
		lines = append(lines, chatLine{text: heading})
		for _, a := range alerts {
			lines = append(lines, chatLine{text: format(a), problem: true})
		}
	}
	add("New:", n.New, func(a Alert) string {
		return fmt.Sprintf("- %s %s: %s (since %s)", strings.ToUpper(string(a.Severity)), a.Object.Label(), a.Reason, chatTime(a.Since))
	})
	add("Still critical:", n.Reminders, func(a Alert) string {
		return fmt.Sprintf("- %s: %s (since %s)", a.Object.Label(), a.Reason, chatTime(a.Since))
	})
	add("Resolved:", n.Resolved, func(a Alert) string {
		return fmt.Sprintf("- %s: %s (since %s, resolved %s)", a.Object.Label(), a.Reason, chatTime(a.Since), chatTime(a.ResolvedAt))
	})
	return lines
}

func joinChatLines(lines []chatLine) string {
	texts := make([]string, 0, len(lines))
	for _, line := range lines {
		texts = append(texts, line.text)
	}
	return strings.Join(texts, "\n")
}

func countProblems(lines []chatLine) int {
	count := 0
	for _, line := range lines {
		if line.problem {
			count++
		}
	}
	return count
}

// moreLine is the last line of a message that had to be cut.
func moreLine(left int, baseURL string) string {
	if baseURL == "" {
		return fmt.Sprintf("… and %d more, see the web UI", left)
	}
	return fmt.Sprintf("… and %d more, see %s", left, baseURL)
}

func chatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(chatTimeFormat)
}

// cutText shortens s to at most limit bytes without splitting a character, and
// marks the cut with "…". It is for single fields with a hard limit, such as
// the Slack header.
func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	end := limit - len(ellipsis)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + ellipsis
}
