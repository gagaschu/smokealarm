package notify

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"

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
	switch {
	case len(n.New) == 1 && len(n.Resolved) == 0:
		a := n.New[0]
		return fmt.Sprintf("%s%s: %s %s", subjectPrefix, strings.ToUpper(string(a.Severity)), a.Reason, objectName(a.Object))
	case len(n.New) == 0 && len(n.Resolved) == 1:
		a := n.Resolved[0]
		return fmt.Sprintf("%sRESOLVED: %s %s", subjectPrefix, a.Reason, objectName(a.Object))
	default:
		return subjectPrefix + summary(n)
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
// resolved ones; the engine has already sorted each list.
const bodyTemplate = `{{.Summary}}
{{if .New}}
NEW PROBLEMS
{{range .New}}
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
	Summary  string
	New      []bodyEntry
	Resolved []bodyEntry
	BaseURL  string
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
		Summary:  summary(n),
		New:      entries(n.New),
		Resolved: entries(n.Resolved),
		BaseURL:  n.BaseURL,
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
			Object:     a.Object.Kind + " " + objectName(a.Object),
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
