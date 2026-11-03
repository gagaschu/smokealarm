package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

func downtimeForm(id int64, until string) url.Values {
	return url.Values{"finding": {strconv.FormatInt(id, 10)}, "until": {until}}
}

func (e *testEnv) activeDowntimes() []store.Downtime {
	e.t.Helper()
	active, err := e.store.ActiveDowntimes(context.Background(), testNow)
	if err != nil {
		e.t.Fatalf("ActiveDowntimes: %v", err)
	}
	return active
}

// resolvedID stores a resolved problem and returns its id.
func (e *testEnv) resolvedID(key string) int64 {
	e.t.Helper()
	e.insert(resolvedProblem(key, "media", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)))
	records, err := e.store.Resolved(context.Background(), "", 10)
	if err != nil {
		e.t.Fatalf("Resolved: %v", err)
	}
	for _, r := range records {
		if r.Key == key {
			return r.ID
		}
	}
	e.t.Fatalf("no resolved problem with key %s", key)
	return 0
}

func TestDowntimeFormPage(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	id := e.firingID("a", checks.Warning)

	rec := e.do(http.MethodGet, "/downtimes/new?finding="+strconv.FormatInt(id, 10), nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`type="datetime-local"`,
		`Until (UTC)`,
		`value="2026-11-03T13:00"`, // now + 1 h, in UTC
		`min="2026-11-03T12:00"`,
		`It is now 2026-11-03 12:00:00 UTC`,
		`A new reason or a new object name`,
		`Set downtime`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form does not contain %q:\n%s", want, body)
		}
	}
	// A reload would throw away what somebody is typing.
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the form must not reload itself")
	}
}

func TestDowntimeFormSuggestsTheRunningDowntime(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	id := e.firingID("a", checks.Critical)
	if rec := e.do(http.MethodPost, "/downtimes", downtimeForm(id, "2026-11-04T08:30"), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}

	body := e.do(http.MethodGet, "/downtimes/new?finding="+strconv.FormatInt(id, 10), nil, cookie).Body.String()
	if !strings.Contains(body, `value="2026-11-04T08:30"`) || !strings.Contains(body, "Change downtime") {
		t.Errorf("form does not offer to change the running downtime:\n%s", body)
	}
}

func TestDowntimeFormErrors(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	if rec := e.do(http.MethodGet, "/downtimes/new?finding=42", nil, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("unknown finding: status = %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/downtimes/new?finding=abc", nil, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid finding: status = %d, want 400", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/downtimes", downtimeForm(42, "2026-11-04T08:30"), cookie); rec.Code != http.StatusNotFound {
		t.Errorf("POST unknown finding: status = %d, want 404", rec.Code)
	}
}

func TestSetDowntimeReadsTheTimeAsUTC(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"minutes", "2026-11-03T12:01", time.Date(2026, 11, 3, 12, 1, 0, 0, time.UTC)},
		{"seconds", "2026-11-03T12:00:01", time.Date(2026, 11, 3, 12, 0, 1, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			id := e.firingID("a", checks.Warning)

			rec := e.do(http.MethodPost, "/downtimes", downtimeForm(id, tt.value), e.login())
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/downtimes" {
				t.Fatalf("status = %d, location = %q, want 303 to /downtimes", rec.Code, rec.Header().Get("Location"))
			}
			active := e.activeDowntimes()
			if len(active) != 1 || !active[0].ExpiresAt.Equal(tt.want) || active[0].Key != "a" {
				t.Errorf("active = %+v, want a until %v", active, tt.want)
			}
		})
	}
}

func TestSetDowntimeRefusesBadTimes(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"in the past", "2026-11-03T11:59", "must be in the future"},
		{"exactly now", "2026-11-03T12:00", "must be in the future"},
		{"empty", "", "Please enter a date and time"},
		{"not a time", "tomorrow", "Please enter a date and time"},
		{"with a time zone", "2026-11-04T08:30Z", "Please enter a date and time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			id := e.firingID("a", checks.Warning)

			rec := e.do(http.MethodPost, "/downtimes", downtimeForm(id, tt.value), e.login())
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("body does not explain the error %q:\n%s", tt.want, rec.Body.String())
			}
			if tt.value != "" && !strings.Contains(rec.Body.String(), `value="`+tt.value+`"`) {
				t.Errorf("the form lost what was entered:\n%s", rec.Body.String())
			}
			if active := e.activeDowntimes(); len(active) != 0 {
				t.Errorf("active = %+v, want no downtime", active)
			}
		})
	}
}

func TestDowntimeForFiringProblemIsMarkedOnTheOverview(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	id := e.firingID("a", checks.Critical)

	before := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if !strings.Contains(before, `/downtimes/new?finding=`+strconv.FormatInt(id, 10)) || strings.Contains(before, "badge downtime") {
		t.Errorf("overview before the downtime:\n%s", before)
	}

	e.do(http.MethodPost, "/downtimes", downtimeForm(id, "2026-11-04T08:30"), cookie)

	after := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	for _, want := range []string{"badge downtime", "2026-11-04 08:30:00 UTC", "1 current problem", "Change downtime"} {
		if !strings.Contains(after, want) {
			t.Errorf("overview does not contain %q:\n%s", want, after)
		}
	}
}

func TestDowntimeFromTheHistory(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	id := e.resolvedID("a")

	before := e.do(http.MethodGet, "/history", nil, cookie).Body.String()
	if !strings.Contains(before, `/downtimes/new?finding=`+strconv.FormatInt(id, 10)) {
		t.Errorf("history offers no downtime:\n%s", before)
	}

	if rec := e.do(http.MethodPost, "/downtimes", downtimeForm(id, "2026-11-04T08:30"), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}

	after := e.do(http.MethodGet, "/history", nil, cookie).Body.String()
	if !strings.Contains(after, "badge downtime") || !strings.Contains(after, "2026-11-04 08:30:00 UTC") {
		t.Errorf("history does not mark the problem:\n%s", after)
	}
}

func TestDowntimesPageListsAndEnds(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	firing := e.firingID("firing", checks.Warning)
	resolved := e.resolvedID("resolved")
	e.do(http.MethodPost, "/downtimes", downtimeForm(firing, "2026-11-03T14:00"), cookie)
	e.do(http.MethodPost, "/downtimes", downtimeForm(resolved, "2026-11-03T15:00"), cookie)

	rec := e.do(http.MethodGet, "/downtimes", nil, cookie)
	body := rec.Body.String()
	for _, want := range []string{"2026-11-03 14:00:00 UTC", "2h 0m", "Pod media/firing", "Pod media/resolved", ">active<", ">not active<", `aria-current="page">Downtimes`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q:\n%s", want, body)
		}
	}

	active := e.activeDowntimes()
	if len(active) != 2 {
		t.Fatalf("active = %+v, want two", active)
	}
	path := "/downtimes/" + strconv.FormatInt(active[0].ID, 10) + "/end"
	if !strings.Contains(body, `action="`+path+`"`) {
		t.Errorf("page has no form for %s:\n%s", path, body)
	}

	end := e.do(http.MethodPost, path, url.Values{}, cookie)
	if end.Code != http.StatusSeeOther || end.Header().Get("Location") != "/downtimes" {
		t.Fatalf("status = %d, location = %q, want 303 to /downtimes", end.Code, end.Header().Get("Location"))
	}
	left := e.activeDowntimes()
	if len(left) != 1 || left[0].Key != "resolved" {
		t.Errorf("active = %+v, want only resolved", left)
	}
	if strings.Contains(e.do(http.MethodGet, "/downtimes", nil, cookie).Body.String(), "Pod media/firing") {
		t.Error("an ended downtime is still listed")
	}
}

func TestDowntimesPageWithoutDowntimes(t *testing.T) {
	e := newTestEnv(t)
	body := e.do(http.MethodGet, "/downtimes", nil, e.login()).Body.String()
	if !strings.Contains(body, "No downtimes are running") {
		t.Errorf("no empty state:\n%s", body)
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the list should reload like the other pages")
	}
}

func TestEndDowntimeInvalidAndUnknownID(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	if rec := e.do(http.MethodPost, "/downtimes/abc/end", url.Values{}, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status = %d, want 400", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/downtimes/42/end", url.Values{}, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("unknown id: status = %d, want 303", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/downtimes/42/end", nil, cookie); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
}

func TestDowntimePagesAndActionsNeedLogin(t *testing.T) {
	e := newTestEnv(t)
	id := e.firingID("a", checks.Warning)

	for _, path := range []string{"/downtimes", "/downtimes/new?finding=" + strconv.FormatInt(id, 10)} {
		rec := e.do(http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("GET %s: status = %d, location = %q, want 303 to /login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/downtimes", "/downtimes/1/end"} {
		rec := e.do(http.MethodPost, path, downtimeForm(id, "2026-11-04T08:30"), nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("POST %s: status = %d, location = %q, want 303 to /login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if active := e.activeDowntimes(); len(active) != 0 {
		t.Errorf("active = %+v, want no downtime", active)
	}
}

func TestDowntimeActionsRefuseOtherOrigins(t *testing.T) {
	for _, value := range []string{"cross-site", "same-site"} {
		t.Run(value, func(t *testing.T) {
			e := newTestEnv(t)
			cookie := e.login()
			id := e.firingID("a", checks.Warning)

			post := func(path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Sec-Fetch-Site", value)
				req.AddCookie(cookie)
				rec := httptest.NewRecorder()
				e.handler.ServeHTTP(rec, req)
				return rec
			}

			rec := post("/downtimes", downtimeForm(id, "2026-11-04T08:30").Encode())
			if rec.Code != http.StatusForbidden {
				t.Errorf("set: status = %d, want 403", rec.Code)
			}
			if active := e.activeDowntimes(); len(active) != 0 {
				t.Errorf("active = %+v, want no downtime", active)
			}

			e.do(http.MethodPost, "/downtimes", downtimeForm(id, "2026-11-04T08:30"), cookie)
			active := e.activeDowntimes()
			if len(active) != 1 {
				t.Fatalf("active = %+v, want one", active)
			}
			rec = post("/downtimes/"+strconv.FormatInt(active[0].ID, 10)+"/end", "")
			if rec.Code != http.StatusForbidden {
				t.Errorf("end: status = %d, want 403", rec.Code)
			}
			if len(e.activeDowntimes()) != 1 {
				t.Error("the downtime was ended by a request from another origin")
			}
		})
	}
}

func TestDowntimeFormBodyIsLimited(t *testing.T) {
	e := newTestEnv(t)
	form := url.Values{"finding": {strings.Repeat("1", formBodyMax+1)}}
	if rec := e.do(http.MethodPost, "/downtimes", form, e.login()); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
