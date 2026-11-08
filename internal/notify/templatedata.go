package notify

import (
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// templateData is what every message template gets: the subject, the mail
// body and the chat text, built in or written by the user. One type for all,
// so a template can be moved from one file to another, and
// docs/message-templates.md describes one set of fields.
type templateData struct {
	Target    string // the kind of target: "mail", "ntfy", "telegram", "slack", "teams"
	Summary   string // the built-in summary line, e.g. "3 new problems, 1 resolved"
	NodesDown []templateProblem
	New       []templateProblem // without the nodes that are down
	Reminders []templateProblem
	Resolved  []templateProblem
	BaseURL   string
	// Omitted is how many problems were left out because the chat text was
	// too long. It is always 0 for subjects and mails.
	Omitted int
}

// templateProblem is one problem as a template sees it. Every text that can
// come from the cluster is on one line already (see oneLine).
type templateProblem struct {
	Severity   string // "critical" or "warning"
	Check      string
	Reason     string
	Kind       string
	Namespace  string // empty for cluster-scoped objects such as nodes
	Name       string
	Object     string // "Pod media/jellyfin"
	Message    string
	Since      time.Time // in SMOKEALARM_TIME_ZONE
	ResolvedAt time.Time // in SMOKEALARM_TIME_ZONE; zero unless resolved
	Duration   string    // how long it lasted, e.g. "3m" or "2h 10m"; empty unless resolved
	Affected   []templateAffected
}

// templateAffected is a problem held back by a node that is down.
type templateAffected struct {
	Object string
	Reason string
}

// newTemplateData prepares a notification for a template of the given kind of
// target.
func newTemplateData(n Notification, kind string) templateData {
	nodes, others := splitNodesDown(n.New)
	return templateData{
		Target:    kind,
		Summary:   summaryLine(n),
		NodesDown: templateProblems(nodes, n.Location),
		New:       templateProblems(others, n.Location),
		Reminders: templateProblems(n.Reminders, n.Location),
		Resolved:  templateProblems(n.Resolved, n.Location),
		BaseURL:   n.BaseURL,
	}
}

// templateProblems converts the alerts, with their times in the given zone
// (nil means UTC).
func templateProblems(alerts []Alert, loc *time.Location) []templateProblem {
	loc = checks.ZoneOrUTC(loc)
	var problems []templateProblem
	for _, a := range alerts {
		p := templateProblem{
			Severity:   string(a.Severity),
			Check:      a.Check,
			Reason:     oneLine(a.Reason),
			Kind:       oneLine(a.Object.Kind),
			Namespace:  oneLine(a.Object.Namespace),
			Name:       oneLine(a.Object.Name),
			Object:     oneLine(a.Object.Label()),
			Message:    oneLine(a.Message),
			Since:      a.Since.In(loc),
			ResolvedAt: a.ResolvedAt.In(loc),
			Duration:   lasted(a.Since, a.ResolvedAt),
		}
		for _, affected := range a.Affected {
			p.Affected = append(p.Affected, templateAffected{
				Object: oneLine(affected.Object.Label()),
				Reason: oneLine(affected.Reason),
			})
		}
		problems = append(problems, p)
	}
	return problems
}

// lasted is how long a resolved problem lasted, as the mails show it: "3m",
// "2h 10m", "less than a minute". It is empty for a problem that is not
// resolved, or whose times make no sense.
func lasted(since, resolvedAt time.Time) string {
	if resolvedAt.IsZero() || since.IsZero() || resolvedAt.Before(since) {
		return ""
	}
	return checks.HumanDuration(resolvedAt.Sub(since))
}

// problemCount is the number of problems in all lists.
func (d templateData) problemCount() int {
	return len(d.NodesDown) + len(d.New) + len(d.Reminders) + len(d.Resolved)
}

// keepFirst returns a copy with only the first keep problems, in the order of
// the message (nodes down, new, reminders, resolved), and counts the rest in
// Omitted. It is how a chat text that is too long gets shorter: the end goes
// first, as when reading it.
func (d templateData) keepFirst(keep int) templateData {
	d.Omitted += d.problemCount() - keep
	lists := []*[]templateProblem{&d.NodesDown, &d.New, &d.Reminders, &d.Resolved}
	for _, list := range lists {
		n := min(len(*list), keep)
		*list = (*list)[:n:n]
		keep -= n
	}
	return d
}
