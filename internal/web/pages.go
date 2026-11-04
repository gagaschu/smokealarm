package web

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

// Limits and formats for the pages.
const (
	historyLimit = 200
	formBodyMax  = 4 << 10 // our forms are tiny; refuse anything bigger
	timeFormat   = "2006-01-02 15:04:05 UTC"

	// historyTimeFormat is shorter than timeFormat to save space in the
	// table; the column header says the times are UTC.
	historyTimeFormat = "2006-01-02 15:04"
)

// page is everything the templates can show. Each page uses its own part.
type page struct {
	Title   string
	Active  string // which navigation entry is highlighted
	ShowNav bool
	Refresh bool // reload the page every 30 seconds

	// login and downtime form
	Error string

	// overview
	Problems          []problemView
	HeldBack          []heldBackView // problems waiting for a down node, by node
	CriticalCount     int // for the counters below the heading
	WarningCount      int
	CanAcknowledgeAny bool // explains the button once, not on every problem
	AcknowledgedAny   bool // the same for removing an acknowledgement
	WaitingFirstRun   bool
	AllGood           bool
	LastRun           string
	LastRunAgo        string

	// history
	History    []historyView
	Namespaces []string
	Namespace  string
	Truncated  bool
	Limit      int

	// status
	Status statusView

	// downtimes
	Downtimes    []downtimeView
	DowntimeForm downtimeFormView
}

type problemView struct {
	ID             int64
	Severity       string // "critical" or "warning", also used as CSS class
	Object         string
	Reason         string
	Message        string
	Since          string // exact time
	Age            string // how long ago
	CanAcknowledge bool   // critical and not acknowledged yet
	AcknowledgedAt string // exact time, empty if not acknowledged
	DowntimeUntil  string // end of the running downtime, empty if there is none
	NotifiesOnly   string // "slack, mail" if not every target hears about it
	NotifiesNobody bool   // no target hears about it (a warning for critical-only targets)
	HoldsBack      int    // for a node that is down: how many problems it holds back
}

// heldBackView is a node that is down and the problems it holds back. It is
// shown also when the node itself is ignored and so not on the page.
type heldBackView struct {
	Node     string
	Problems []problemView // only Severity, Object, Reason, Message, Since and Age are set
}

type historyView struct {
	ID             int64
	Severity       string
	Object         string
	Reason         string
	Message        string
	ResolvedAt     string
	Lasted         string
	DowntimeUntil  string // end of the running downtime, empty if there is none
	NotifiesOnly   string // as in problemView
	NotifiesNobody bool
}

type statusView struct {
	LastRun      string
	LastRunError string
	Checks       []checkView
	Targets      []targetView
	LogOnly      bool // no notification target is configured
}

// targetView is one notification target. The description never contains a
// secret; the engine gets it from the notifier.
type targetView struct {
	Description  string
	CriticalOnly bool   // hears only about critical problems
	LastSentAt   string // empty if nothing was sent since the start
	LastError    string
	LastErrorAt  string
}

type checkView struct {
	Name   string
	Active bool
	Reason string
}

// render writes a page. Pages show cluster state, so browsers must not cache
// them.
func (s *Server) render(w http.ResponseWriter, status int, name string, data page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.pages[name].ExecuteTemplate(w, "layout", data); err != nil {
		// The status line is already sent; all we can do is log it.
		s.Logger.Error("render page", "page", name, "error", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.Logger.Error(what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// loginPage shows the login form, or skips it if there is a session already.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok, err := s.Sessions.Valid(r); err == nil && ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", page{Title: "Log in"})
}

// loginSubmit checks the credentials and starts a session. After a login
// the user always lands on the overview; there is no "next" parameter, so
// nobody can be redirected to another site.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	client := clientIP(r)
	if !s.Limiter.Allowed(client) {
		// No password check at all, so a blocked client learns nothing.
		s.Logger.Warn("login blocked after too many failures", "client", client)
		s.render(w, http.StatusTooManyRequests, "login", page{
			Title: "Log in", Error: "Too many failed attempts. Please try again later.",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, formBodyMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username, password := r.PostFormValue("username"), r.PostFormValue("password")

	if !s.Login.Verify(username, password) {
		s.Limiter.Failed(client)
		// The password is never logged; the user name is not a secret.
		s.Logger.Warn("login failed", "client", client, "username", username)
		s.render(w, http.StatusUnauthorized, "login", page{
			Title: "Log in", Error: "Invalid username or password.",
		})
		return
	}

	s.Limiter.Succeeded(client)
	if err := s.Sessions.Start(w, r, s.Login.Username); err != nil {
		s.serverError(w, "start session", err)
		return
	}
	s.Logger.Info("login succeeded", "client", client, "username", s.Login.Username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.End(w, r); err != nil {
		s.serverError(w, "end session", err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// overview lists the problems that are firing, worst first.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	records, err := s.Store.OpenRecords(r.Context())
	if err != nil {
		s.serverError(w, "load open findings", err)
		return
	}
	// Pending problems are not shown: they have not lasted long enough to
	// be reported, so listing them here would contradict the mails. Those
	// held back by a down node are the exception: they are known, they just
	// are not reported on their own.
	held := engine.HeldBack(records)
	var firing []store.Record
	for _, rec := range records {
		if rec.State == store.StateFiring {
			firing = append(firing, rec)
		}
	}
	sortProblems(firing)

	now := s.Now()
	downtimeEnds, err := s.downtimeEnds(r.Context(), now)
	if err != nil {
		s.serverError(w, "load downtimes", err)
		return
	}
	data := page{Title: "Overview", Active: "overview", ShowNav: true, Refresh: true}
	for _, rec := range firing {
		view := newProblemView(rec, now)
		view.NotifiesOnly, view.NotifiesNobody = s.audience(rec)
		view.DowntimeUntil = downtimeEnds[rec.Key]
		if rec.Check == checks.CheckNodeNotReady {
			view.HoldsBack = len(held[rec.Object.Name])
		}
		data.Problems = append(data.Problems, view)
		if rec.Severity == checks.Critical {
			data.CriticalCount++
		} else {
			data.WarningCount++
		}
		data.CanAcknowledgeAny = data.CanAcknowledgeAny || view.CanAcknowledge
		data.AcknowledgedAny = data.AcknowledgedAny || view.AcknowledgedAt != ""
	}

	data.HeldBack = heldBackViews(held, now)

	lastRun := s.Status().LastRun
	data.WaitingFirstRun = lastRun.IsZero()
	data.AllGood = !data.WaitingFirstRun && len(data.Problems) == 0 && len(data.HeldBack) == 0
	if !lastRun.IsZero() {
		data.LastRun = lastRun.UTC().Format(timeFormat)
		data.LastRunAgo = formatAge(now.Sub(lastRun))
	}
	s.render(w, http.StatusOK, "overview", data)
}

// audience returns what the pages say about who hears about a problem: nothing
// if every target does, otherwise the targets that do, or that nobody does.
func (s *Server) audience(rec store.Record) (only string, nobody bool) {
	if s.Audience == nil {
		return "", false
	}
	targets, all := s.Audience(rec)
	switch {
	case all:
		return "", false
	case len(targets) == 0:
		return "", true
	}
	return strings.Join(targets, ", "), false
}

// heldBackViews sorts the held-back problems by node name, and within a node
// like the problems.
func heldBackViews(held map[string][]store.Record, now time.Time) []heldBackView {
	var views []heldBackView
	for node, records := range held {
		sortProblems(records)
		view := heldBackView{Node: node}
		for _, rec := range records {
			view.Problems = append(view.Problems, newProblemView(rec, now))
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Node < views[j].Node })
	return views
}

// newProblemView prepares one firing problem for the overview.
func newProblemView(rec store.Record, now time.Time) problemView {
	view := problemView{
		ID:             rec.ID,
		Severity:       string(rec.Severity),
		Object:         rec.Object.Label(),
		Reason:         rec.Reason,
		Message:        rec.Message,
		Since:          rec.FirstSeen.UTC().Format(timeFormat),
		Age:            formatAge(now.Sub(rec.FirstSeen)),
		CanAcknowledge: rec.Severity == checks.Critical && rec.AcknowledgedAt == nil,
	}
	if rec.AcknowledgedAt != nil {
		view.AcknowledgedAt = rec.AcknowledgedAt.UTC().Format(timeFormat)
	}
	return view
}

// acknowledge marks a firing critical problem as known, which stops its
// reminders until it is resolved. It always returns to the overview: if the
// problem is gone or changed in the meantime, the page shows that.
func (s *Server) acknowledge(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, changed, err := s.Store.Acknowledge(r.Context(), id, s.Now())
	if err != nil {
		s.serverError(w, "acknowledge finding", err)
		return
	}
	if changed {
		s.Logger.Info("finding acknowledged", "key", key)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// unacknowledge takes an acknowledgement back, which can bring back reminders
// right away. It also returns to the overview in every case.
func (s *Server) unacknowledge(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, changed, err := s.Store.Unacknowledge(r.Context(), id)
	if err != nil {
		s.serverError(w, "remove acknowledgement", err)
		return
	}
	if changed {
		s.Logger.Info("finding acknowledgement removed", "key", key)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sortProblems puts nodes that are down first, because the problems around
// them are often their consequences, then critical problems, then the
// longest-lasting ones.
func sortProblems(records []store.Record) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		aNode, bNode := a.Check == checks.CheckNodeNotReady, b.Check == checks.CheckNodeNotReady
		if aNode != bNode {
			return aNode
		}
		if a.Severity != b.Severity {
			return a.Severity == checks.Critical
		}
		if !a.FirstSeen.Equal(b.FirstSeen) {
			return a.FirstSeen.Before(b.FirstSeen)
		}
		return a.Key < b.Key
	})
}

// history lists resolved problems, optionally for one namespace.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")

	// One more than shown, to know whether the list was cut off.
	records, err := s.Store.Resolved(r.Context(), namespace, historyLimit+1)
	if err != nil {
		s.serverError(w, "load history", err)
		return
	}
	namespaces, err := s.Store.Namespaces(r.Context())
	if err != nil {
		s.serverError(w, "load namespaces", err)
		return
	}
	downtimeEnds, err := s.downtimeEnds(r.Context(), s.Now())
	if err != nil {
		s.serverError(w, "load downtimes", err)
		return
	}

	data := page{
		Title: "History", Active: "history", ShowNav: true, Refresh: true,
		Namespaces: namespaces, Namespace: namespace, Limit: historyLimit,
	}
	if len(records) > historyLimit {
		records, data.Truncated = records[:historyLimit], true
	}
	for _, rec := range records {
		view := historyView{
			ID:            rec.ID,
			Severity:      string(rec.Severity),
			Object:        rec.Object.Label(),
			Reason:        rec.Reason,
			Message:       rec.Message,
			DowntimeUntil: downtimeEnds[rec.Key],
		}
		view.NotifiesOnly, view.NotifiesNobody = s.audience(rec)
		if rec.ResolvedAt != nil {
			view.ResolvedAt = rec.ResolvedAt.UTC().Format(historyTimeFormat)
			view.Lasted = formatAge(rec.ResolvedAt.Sub(rec.FirstSeen))
		}
		data.History = append(data.History, view)
	}
	s.render(w, http.StatusOK, "history", data)
}

// status shows how smokealarm itself is doing.
func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.Status()

	view := statusView{
		LastRun:      formatOptionalTime(st.LastRun),
		LastRunError: st.LastRunError,
		Targets:      targetViews(st.Targets),
	}
	view.LogOnly = len(st.Targets) == 1 && st.Targets[0].Name == "log"
	for _, c := range st.Checks {
		view.Checks = append(view.Checks, checkView{Name: c.Name, Active: c.Active, Reason: c.Reason})
	}
	s.render(w, http.StatusOK, "status", page{
		Title: "Status", Active: "status", ShowNav: true, Refresh: true, Status: view,
	})
}

func targetViews(targets []engine.TargetStatus) []targetView {
	var views []targetView
	for _, t := range targets {
		views = append(views, targetView{
			Description:  t.Description,
			CriticalOnly: t.CriticalOnly,
			LastSentAt:   formatOptionalTime(t.LastSentAt),
			LastError:    t.LastError,
			LastErrorAt:  formatOptionalTime(t.LastErrorAt),
		})
	}
	return views
}

// formatOptionalTime is the time in UTC, or empty for the zero time.
func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// formatAge writes a duration in the two most useful units: "3d 4h",
// "2h 10m", "5m".
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// clientIP is the address of the peer. X-Forwarded-For is not used because
// any client can forge it; behind a proxy all clients therefore share one
// limit, which is fine for a single admin user.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
