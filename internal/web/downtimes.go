package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gagaschu/smokealarm/internal/store"
)

// Time layouts of an <input type="datetime-local">. The browser sends no time
// zone, so the form says "UTC" and the value is read as UTC.
const (
	dateTimeLocalFormat        = "2006-01-02T15:04"
	dateTimeLocalSecondsFormat = "2006-01-02T15:04:05"
)

// defaultDowntime is what the form suggests for a new downtime.
const defaultDowntime = time.Hour

type downtimeView struct {
	ID        int64
	Object    string
	Reason    string
	Until     string
	Remaining string
	Firing    bool // the problem is firing right now
}

type downtimeFormView struct {
	FindingID int64
	Object    string
	Reason    string
	Until     string // value of the input field
	Min       string // the current minute: earlier values are in the past
	Now       string // current time, so the reader can compare it with UTC
	Existing  string // end of the downtime that would be replaced, if any
}

// downtimeEnds maps the key of each running downtime to its end, for the marks
// on the overview and the history.
func (s *Server) downtimeEnds(ctx context.Context, now time.Time) (map[string]string, error) {
	active, err := s.Store.ActiveDowntimes(ctx, now)
	if err != nil {
		return nil, err
	}
	ends := map[string]string{}
	for _, d := range active {
		ends[d.Key] = d.ExpiresAt.UTC().Format(timeFormat)
	}
	return ends, nil
}

// downtimes lists the running downtimes.
func (s *Server) downtimes(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	active, err := s.Store.ActiveDowntimes(r.Context(), now)
	if err != nil {
		s.serverError(w, "load downtimes", err)
		return
	}
	open, err := s.Store.OpenRecords(r.Context())
	if err != nil {
		s.serverError(w, "load open findings", err)
		return
	}
	firing := map[string]bool{}
	for _, rec := range open {
		firing[rec.Key] = rec.State == store.StateFiring
	}

	data := page{Title: "Downtimes", Active: "downtimes", ShowNav: true, Refresh: true}
	for _, d := range active {
		data.Downtimes = append(data.Downtimes, downtimeView{
			ID:        d.ID,
			Object:    d.Object.Label(),
			Reason:    d.Reason,
			Until:     d.ExpiresAt.UTC().Format(timeFormat),
			Remaining: formatAge(d.ExpiresAt.Sub(now)),
			Firing:    firing[d.Key],
		})
	}
	s.render(w, http.StatusOK, "downtimes", data)
}

// newDowntime shows the form for one problem. It is a page of its own because
// the overview and the history reload every 30 seconds, which would throw away
// what somebody is typing.
func (s *Server) newDowntime(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.loadFinding(w, r, r.URL.Query().Get("finding"))
	if !ok {
		return
	}
	s.renderDowntimeForm(w, r, rec, "", "", http.StatusOK)
}

// createDowntime sets the downtime for the problem of the given finding, or
// replaces the one it has. An end that is not in the future is refused.
func (s *Server) createDowntime(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, formBodyMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rec, ok := s.loadFinding(w, r, r.PostFormValue("finding"))
	if !ok {
		return
	}

	now := s.Now()
	value := r.PostFormValue("until")
	until, problem := parseUntil(value, now)
	if problem != "" {
		s.renderDowntimeForm(w, r, rec, value, problem, http.StatusBadRequest)
		return
	}

	err := s.Store.SetDowntime(r.Context(), store.Downtime{
		Key:       rec.Key,
		Check:     rec.Check,
		Object:    rec.Object,
		Reason:    rec.Reason,
		CreatedAt: now,
		ExpiresAt: until,
	})
	if err != nil {
		s.serverError(w, "set downtime", err)
		return
	}
	s.Logger.Info("downtime set", "key", rec.Key, "until", until.Format(timeFormat))
	http.Redirect(w, r, "/downtimes", http.StatusSeeOther)
}

// endDowntime ends a running downtime now. If the problem is still firing, the
// engine reports it in its next run.
func (s *Server) endDowntime(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, changed, err := s.Store.EndDowntime(r.Context(), id, s.Now())
	if err != nil {
		s.serverError(w, "end downtime", err)
		return
	}
	if changed {
		s.Logger.Info("downtime ended early", "key", key)
	}
	http.Redirect(w, r, "/downtimes", http.StatusSeeOther)
}

// loadFinding finds the problem a downtime is for. If that fails it has already
// answered the request and returns false.
func (s *Server) loadFinding(w http.ResponseWriter, r *http.Request, value string) (store.Record, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return store.Record{}, false
	}
	rec, found, err := s.Store.Record(r.Context(), id)
	if err != nil {
		s.serverError(w, "load finding", err)
		return store.Record{}, false
	}
	if !found {
		http.NotFound(w, r)
		return store.Record{}, false
	}
	return rec, true
}

// renderDowntimeForm shows the form. An empty until means nothing was entered
// yet: it then suggests the end of the running downtime, or a new one.
func (s *Server) renderDowntimeForm(w http.ResponseWriter, r *http.Request, rec store.Record, until, errorText string, status int) {
	now := s.Now().UTC()
	active, err := s.Store.ActiveDowntimes(r.Context(), now)
	if err != nil {
		s.serverError(w, "load downtimes", err)
		return
	}

	form := downtimeFormView{
		FindingID: rec.ID,
		Object:    rec.Object.Label(),
		Reason:    rec.Reason,
		Until:     until,
		Min:       now.Format(dateTimeLocalFormat),
		Now:       now.Format(timeFormat),
	}
	if until == "" {
		form.Until = now.Add(defaultDowntime).Format(dateTimeLocalFormat)
	}
	for _, d := range active {
		if d.Key != rec.Key {
			continue
		}
		form.Existing = d.ExpiresAt.UTC().Format(timeFormat)
		if until == "" {
			form.Until = d.ExpiresAt.UTC().Format(dateTimeLocalFormat)
		}
	}
	s.render(w, status, "downtime_form", page{
		Title: "Downtime", Active: "downtimes", ShowNav: true, Error: errorText, DowntimeForm: form,
	})
}

// parseUntil reads the value of the form field as UTC and checks that it lies
// in the future. If it does not, the second result is a message for the user.
func parseUntil(value string, now time.Time) (time.Time, string) {
	until, err := time.Parse(dateTimeLocalFormat, value)
	if err != nil {
		until, err = time.Parse(dateTimeLocalSecondsFormat, value)
	}
	if err != nil {
		return time.Time{}, "Please enter a date and time."
	}
	if !until.After(now) {
		return time.Time{}, "The end of a downtime must be in the future."
	}
	return until, ""
}
