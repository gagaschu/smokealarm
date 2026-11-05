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

// Subject summarizes the notification in one line. A node that is newly down
// comes first ("NODE DOWN: pi-2 (+ 1 resolved)"), because the problems around
// it are its consequences. Otherwise a single change is named ("CRITICAL:
// CrashLoopBackOff media/jellyfin") and several changes are counted ("3 new
// problems, 1 resolved").
func Subject(n Notification) string {
	if nodes, _ := splitNodesDown(n.New); len(nodes) > 0 || len(n.New)+len(n.Reminders)+len(n.Resolved) != 1 {
		return subjectPrefix + summaryLine(n)
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

// objectName is "namespace/name", or just "name" for cluster-scoped objects
// such as nodes.
func objectName(ref checks.ObjectRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "/" + ref.Name
}

// bodyTemplate is the plaintext mail body. Nodes that are newly down come
// first, with the problems they hold back, then the other new problems, then
// reminders, then resolved ones; the engine has already sorted each list.
const bodyTemplate = `{{define "affected"}}{{if .Affected}}  Affected, not reported on their own while the node is down:
{{range .Affected}}    - {{.}}
{{end}}{{end}}{{end}}{{.Summary}}
{{if .NodesDown}}
NODE DOWN
{{range .NodesDown}}
[{{.Severity}}] {{.Reason}}
  Object:  {{.Object}}
  Message: {{.Message}}
  Since:   {{.Since}}
{{template "affected" .}}{{end}}{{end}}{{if .New}}
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
{{template "affected" .}}{{end}}{{end}}{{if .Resolved}}
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
	NodesDown []bodyEntry
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
	Affected   []string // "Deployment media/sonarr: ReplicasUnavailable"
}

// Body renders the plaintext mail body.
func Body(n Notification) (string, error) {
	tmpl, err := template.New("body").Parse(bodyTemplate)
	if err != nil {
		return "", fmt.Errorf("parse body template: %w", err)
	}
	nodes, others := splitNodesDown(n.New)
	data := bodyData{
		Summary:   summaryLine(n),
		NodesDown: entries(nodes),
		New:       entries(others),
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
			Message:    oneLine(a.Message),
			Since:      formatTime(a.Since),
			ResolvedAt: formatTime(a.ResolvedAt),
			Affected:   affectedLines(a.Affected),
		})
	}
	return result
}

// oneLine puts a text on a single line. Part of a message comes from the
// cluster (image pull errors, event texts), and whoever can write there could
// otherwise add lines that look like a section of the mail or a link.
// strings.Fields splits at every kind of whitespace, also at Unicode line
// breaks.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func affectedLines(affected []AffectedProblem) []string {
	var lines []string
	for _, p := range affected {
		lines = append(lines, p.Object.Label()+": "+p.Reason)
	}
	return lines
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
	nodes, others := splitNodesDown(n.New)
	add("Node down:", nodes, func(a Alert) string {
		return fmt.Sprintf("- %s (since %s)%s", a.Object.Label(), chatTime(a.Since), affectedCount(a))
	})
	add("New:", others, func(a Alert) string {
		return fmt.Sprintf("- %s %s: %s (since %s)", strings.ToUpper(string(a.Severity)), a.Object.Label(), a.Reason, chatTime(a.Since))
	})
	add("Still critical:", n.Reminders, func(a Alert) string {
		return fmt.Sprintf("- %s: %s (since %s)%s", a.Object.Label(), a.Reason, chatTime(a.Since), affectedCount(a))
	})
	add("Resolved:", n.Resolved, func(a Alert) string {
		return fmt.Sprintf("- %s: %s (since %s, resolved %s)", a.Object.Label(), a.Reason, chatTime(a.Since), chatTime(a.ResolvedAt))
	})
	return lines
}

// affectedCount is ", 3 affected" for a node that holds back problems.
func affectedCount(a Alert) string {
	if len(a.Affected) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d affected", len(a.Affected))
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
