package web

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gagaschu/smokealarm/internal/auth"
	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// testEnv is a server with a real SQLite file and real sessions; only the
// engine status and readiness are faked.
type testEnv struct {
	t       *testing.T
	store   *store.Store
	handler http.Handler
	status  engine.Status
	ready   error
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t}
	now := func() time.Time { return testNow }

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"), now)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.store = db

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Ready:    func(context.Context) error { return e.ready },
		Store:    db,
		Login:    auth.PasswordLogin{Username: "admin", Hash: hash},
		Sessions: &auth.Sessions{Store: db, TTL: time.Hour, SecureCookie: true, Now: now},
		Limiter:  &auth.RateLimiter{Now: now},
		Status:   func() engine.Status { return e.status },
		Now:      now,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	e.handler, err = server.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return e
}

// do sends a request through the handler.
func (e *testEnv) do(method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func loginForm(user, password string) url.Values {
	return url.Values{"username": {user}, "password": {password}}
}

// login logs in as the admin and returns the session cookie.
func (e *testEnv) login() *http.Cookie {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/login", loginForm("admin", "secret"), nil)
	if rec.Code != http.StatusSeeOther {
		e.t.Fatalf("login status = %d, want 303", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "smokealarm_session" {
			return c
		}
	}
	e.t.Fatal("login did not set a session cookie")
	return nil
}

func (e *testEnv) insert(records ...store.Record) {
	e.t.Helper()
	if err := e.store.Apply(context.Background(), store.Changes{Insert: records}); err != nil {
		e.t.Fatalf("insert: %v", err)
	}
}

func problem(key, namespace string, severity checks.Severity, state store.State, firstSeen time.Time, message string) store.Record {
	return store.Record{
		Finding: checks.Finding{
			Key: key, Check: "test", Severity: severity,
			Object:  checks.ObjectRef{Kind: "Pod", Namespace: namespace, Name: key},
			Reason:  "Broken",
			Message: message,
		},
		State:     state,
		FirstSeen: firstSeen,
		LastSeen:  testNow,
	}
}

func resolvedProblem(key, namespace string, firstSeen, resolvedAt time.Time) store.Record {
	r := problem(key, namespace, checks.Warning, store.StateResolved, firstSeen, "was broken")
	r.ResolvedAt = &resolvedAt
	return r
}

func TestHealthz(t *testing.T) {
	e := newTestEnv(t)
	e.ready = errors.New("not ready") // liveness must not depend on readiness
	if rec := e.do(http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.do(http.MethodGet, "/readyz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("ready: status = %d, want 200", rec.Code)
	}

	e.ready = errors.New("caches not synced")
	rec := e.do(http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "caches not synced") {
		t.Errorf("not ready: status = %d, body %q; want 503 with the reason", rec.Code, rec.Body.String())
	}
}

func TestPagesRequireLogin(t *testing.T) {
	e := newTestEnv(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/history"},
		{http.MethodGet, "/status"},
		{http.MethodPost, "/logout"},
	} {
		rec := e.do(tt.method, tt.path, nil, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s %s: status %d, location %q; want a redirect to /login",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestStaticFilesAndProbesNeedNoLogin(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodGet, "/static/style.css", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("style.css: status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := e.do(http.MethodGet, "/static/missing.css", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing file: status %d, want 404", rec.Code)
	}
	// The browser asks for the icon before anybody has logged in.
	rec = e.do(http.MethodGet, "/static/favicon.svg", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/svg+xml") {
		t.Errorf("favicon.svg: status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// The content security policy blocks inline styles, so a style attribute in
// a template would silently do nothing in the browser.
func TestTemplatesHaveNoStyleAttributes(t *testing.T) {
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v, %v", files, err)
	}
	for _, name := range files {
		content, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "style=") {
			t.Errorf("%s has a style attribute; put the rule into static/style.css", name)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.do(http.MethodGet, "/nope", nil, e.login()); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLoginPage(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodGet, "/login", nil, nil)
	body := rec.Body.String()

	if rec.Code != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Fatalf("status %d, body %q; want the login form", rec.Code, body)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the login page must not refresh itself: it would wipe what the user typed")
	}
	if strings.Contains(body, "Log out") {
		t.Error("the login page must not show the navigation")
	}
}

func TestLoginWithWrongCredentials(t *testing.T) {
	e := newTestEnv(t)
	for _, form := range []url.Values{
		loginForm("admin", "wrong"),
		loginForm("root", "secret"),
		loginForm("", ""),
	} {
		rec := e.do(http.MethodPost, "/login", form, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%v: status = %d, want 401", form, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Invalid username or password.") {
			t.Errorf("%v: body does not show the generic error", form)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%v: a session cookie was set", form)
		}
	}
}

func TestLoginSuccessAndSession(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodPost, "/login", loginForm("admin", "secret"), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, location %q; want a redirect to /", rec.Code, rec.Header().Get("Location"))
	}

	cookie := e.login()
	if rec := e.do(http.MethodGet, "/", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("overview with a session: status %d, want 200", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/login", nil, cookie); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("login page with a session: status %d, want a redirect to /", rec.Code)
	}
}

func TestLoginIgnoresRedirectTargets(t *testing.T) {
	e := newTestEnv(t)
	form := loginForm("admin", "secret")
	form.Set("next", "https://evil.example.com")
	rec := e.do(http.MethodPost, "/login?next=https://evil.example.com", form, nil)
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want always /", got)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 5; i++ {
		if rec := e.do(http.MethodPost, "/login", loginForm("admin", "wrong"), nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, rec.Code)
		}
	}
	// Even the right password is refused now, without being checked.
	rec := e.do(http.MethodPost, "/login", loginForm("admin", "secret"), nil)
	if rec.Code != http.StatusTooManyRequests || len(rec.Result().Cookies()) != 0 {
		t.Errorf("status %d, cookies %v; want 429 and no session", rec.Code, rec.Result().Cookies())
	}
}

func TestLoginBodyIsLimited(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodPost, "/login", url.Values{"password": {strings.Repeat("x", 10_000)}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized form", rec.Code)
	}
}

func TestLogout(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()

	rec := e.do(http.MethodPost, "/logout", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status %d, location %q; want a redirect to /login", rec.Code, rec.Header().Get("Location"))
	}
	// The browser would drop the cookie, but a copy of it must be useless too.
	if rec := e.do(http.MethodGet, "/", nil, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("old session still works after logout: status %d", rec.Code)
	}
	// Logging out is a POST: a link or image on another site cannot trigger it.
	if rec := e.do(http.MethodGet, "/logout", nil, e.login()); rec.Code == http.StatusSeeOther && rec.Header().Get("Location") == "/login" {
		t.Error("GET /logout must not log out")
	}
}

func TestOverviewWaitingForFirstRun(t *testing.T) {
	e := newTestEnv(t)
	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if !strings.Contains(body, "Waiting for the first check run") || strings.Contains(body, "All good") {
		t.Errorf("body = %q; before the first run nothing is known, so it must not say all good", body)
	}
}

func TestOverviewAllGood(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow.Add(-5 * time.Minute)
	e.insert(problem("pending-one", "media", checks.Critical, store.StatePending, testNow, "not yet"))

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	for _, want := range []string{"All good", "Last check:", "5m ago", "2026-11-03 11:55:00 UTC"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "pending-one") {
		t.Error("a pending problem is not reported yet and must not be listed")
	}
}

func TestOverviewListsFiringProblemsWorstFirst(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	e.insert(
		problem("warn-old", "media", checks.Warning, store.StateFiring, testNow.Add(-5*time.Hour), "w"),
		problem("crit-new", "media", checks.Critical, store.StateFiring, testNow.Add(-1*time.Hour), "c1"),
		problem("crit-old", "infra", checks.Critical, store.StateFiring, testNow.Add(-2*time.Hour), "c2"),
	)

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()

	order := []string{"crit-old", "crit-new", "warn-old"}
	last := -1
	for _, key := range order {
		i := strings.Index(body, key)
		if i < 0 || i < last {
			t.Fatalf("%s is missing or out of order (want %v: critical first, longest-lasting first):\n%s", key, order, body)
		}
		last = i
	}
	for _, want := range []string{"3 current problems", "Pod infra/crit-old", "2h 0m ago", `class="badge critical"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
}

func TestOverviewCountsProblemsBySeverity(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	e.insert(
		problem("crit-a", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "c"),
		problem("crit-b", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "c"),
		problem("warn", "media", checks.Warning, store.StateFiring, testNow.Add(-time.Hour), "w"),
		// Pending problems are not on the page, so they are not counted.
		problem("pending", "media", checks.Critical, store.StatePending, testNow, "p"),
	)
	body := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	for _, want := range []string{`<span class="count critical">2 critical</span>`, `<span class="count warning">1 warning</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview does not contain %q:\n%s", want, body)
		}
	}

	only := newTestEnv(t)
	only.status.LastRun = testNow
	only.insert(
		problem("warn-a", "media", checks.Warning, store.StateFiring, testNow, "w"),
		problem("warn-b", "media", checks.Warning, store.StateFiring, testNow, "w"),
	)
	body = only.do(http.MethodGet, "/", nil, only.login()).Body.String()
	if !strings.Contains(body, ">2 warnings<") || strings.Contains(body, "count critical") {
		t.Errorf("want only the warning counter, in plural:\n%s", body)
	}
}

func TestOverviewEscapesMessages(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	e.insert(problem("evil", "media", checks.Warning, store.StateFiring, testNow, `<script>alert(1)</script>`))

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Errorf("a message from the cluster must be escaped:\n%s", body)
	}
}

func TestOverviewSingularProblem(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	e.insert(problem("one", "media", checks.Warning, store.StateFiring, testNow, "m"))
	if body := e.do(http.MethodGet, "/", nil, e.login()).Body.String(); !strings.Contains(body, "1 current problem<") {
		t.Errorf("want the singular heading:\n%s", body)
	}
}

func TestHistoryAndNamespaceFilter(t *testing.T) {
	e := newTestEnv(t)
	e.insert(
		resolvedProblem("in-media", "media", testNow.Add(-3*time.Hour), testNow.Add(-2*time.Hour)),
		resolvedProblem("in-infra", "infra", testNow.Add(-5*time.Hour), testNow.Add(-4*time.Hour)),
		problem("still-open", "media", checks.Warning, store.StateFiring, testNow, "open"),
	)
	cookie := e.login()

	all := e.do(http.MethodGet, "/history", nil, cookie).Body.String()
	for _, want := range []string{"in-media", "in-infra", `<option value="infra">`, `<option value="media">`, "1h 0m"} {
		if !strings.Contains(all, want) {
			t.Errorf("unfiltered history does not contain %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "still-open") {
		t.Error("an open problem is not history")
	}
	if strings.Index(all, "in-media") > strings.Index(all, "in-infra") {
		t.Error("history must show the most recently resolved problem first")
	}

	filtered := e.do(http.MethodGet, "/history?namespace=media", nil, cookie).Body.String()
	if !strings.Contains(filtered, "in-media") || strings.Contains(filtered, "in-infra") {
		t.Errorf("filter by namespace failed:\n%s", filtered)
	}
	if !strings.Contains(filtered, `<option value="media" selected>`) {
		t.Error("the chosen namespace must stay selected")
	}

	empty := e.do(http.MethodGet, "/history?namespace=nothing", nil, cookie).Body.String()
	if !strings.Contains(empty, "No resolved problems in this namespace") {
		t.Errorf("want the empty message:\n%s", empty)
	}
}

func TestHistoryNamespaceIsEscaped(t *testing.T) {
	e := newTestEnv(t)
	body := e.do(http.MethodGet, `/history?namespace="><script>alert(1)</script>`, nil, e.login()).Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Errorf("the namespace parameter must not reach the page unescaped:\n%s", body)
	}
}

func TestStatusPage(t *testing.T) {
	e := newTestEnv(t)
	e.status = engine.Status{
		LastRun:      testNow.Add(-time.Minute),
		LastRunError: "save findings: disk full",
		Targets: []engine.TargetStatus{
			{
				Name: "mail", Description: "mail (smtp.example.com)",
				LastError:   "mail: connect to smtp.example.com:587: timeout",
				LastErrorAt: testNow.Add(-2 * time.Minute),
			},
			{Name: "ntfy", Description: "ntfy (ntfy.example.org)", LastSentAt: testNow.Add(-3 * time.Minute)},
			{Name: "slack", Description: "slack"},
		},
		Checks: []engine.CheckStatus{
			{Name: "pod-crashloop", Active: true},
			{Name: "argocd-health", Active: false, Reason: "Argo CD CRDs not installed"},
		},
	}

	body := e.do(http.MethodGet, "/status", nil, e.login()).Body.String()
	for _, want := range []string{
		"2026-11-03 11:59:00 UTC", "save findings: disk full",
		"mail (smtp.example.com)", "sending failed",
		"mail: connect to smtp.example.com:587: timeout", "2026-11-03 11:58:00 UTC",
		"ntfy (ntfy.example.org)", "2026-11-03 11:57:00 UTC", "slack", "nothing since the start",
		"pod-crashloop", "active", "argocd-health", "skipped", "Argo CD CRDs not installed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page does not contain %q:\n%s", want, body)
		}
	}
}

func TestStatusPageWithoutMailAndErrors(t *testing.T) {
	e := newTestEnv(t)
	e.status = engine.Status{Targets: []engine.TargetStatus{{Name: "log", Description: "log only"}}}

	body := e.do(http.MethodGet, "/status", nil, e.login()).Body.String()
	if !strings.Contains(body, "go to the log only") || !strings.Contains(body, "not yet") {
		t.Errorf("unexpected status page:\n%s", body)
	}
	if strings.Contains(body, "sending failed") || strings.Contains(body, "Last run error") {
		t.Error("error rows must only appear when there is an error")
	}
}

func TestAutoRefreshOnlyOnPages(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	for _, path := range []string{"/", "/history", "/status"} {
		if body := e.do(http.MethodGet, path, nil, cookie).Body.String(); !strings.Contains(body, `<meta http-equiv="refresh" content="30">`) {
			t.Errorf("%s has no 30 second refresh", path)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	for _, path := range []string{"/login", "/", "/healthz", "/static/style.css"} {
		h := e.do(http.MethodGet, path, nil, cookie).Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: missing security headers: %v", path, h)
		}
	}
	if got := e.do(http.MethodGet, "/login", nil, nil).Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("/login: Cache-Control = %q, want no-store", got)
	}
	for _, path := range []string{"/", "/history", "/status"} {
		if got := e.do(http.MethodGet, path, nil, cookie).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestSessionCookieIsHardened(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v, want HttpOnly, Secure and SameSite=Lax", cookie)
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "less than a minute"},
		{time.Minute, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h 0m"},
		{2*time.Hour + 10*time.Minute, "2h 10m"},
		{24 * time.Hour, "1d 0h"},
		{3*24*time.Hour + 4*time.Hour + 30*time.Minute, "3d 4h"},
		{-time.Minute, "less than a minute"},
	}
	for _, tt := range tests {
		if got := formatAge(tt.d); got != tt.want {
			t.Errorf("formatAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// firingID stores a firing problem and returns its id.
func (e *testEnv) firingID(key string, severity checks.Severity) int64 {
	e.t.Helper()
	e.insert(problem(key, "media", severity, store.StateFiring, testNow.Add(-time.Hour), "it is broken"))
	return e.openRecord(key).ID
}

// openRecord returns the open problem with this key.
func (e *testEnv) openRecord(key string) store.Record {
	e.t.Helper()
	records, err := e.store.OpenRecords(context.Background())
	if err != nil {
		e.t.Fatalf("OpenRecords: %v", err)
	}
	for _, r := range records {
		if r.Key == key {
			return r
		}
	}
	e.t.Fatalf("no open problem with key %s", key)
	return store.Record{}
}

func acknowledgePath(id int64) string {
	return "/problems/" + strconv.FormatInt(id, 10) + "/acknowledge"
}

func TestAcknowledgeFromOverview(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	id := e.firingID("a", checks.Critical)

	before := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if !strings.Contains(before, `action="`+acknowledgePath(id)+`"`) {
		t.Errorf("overview has no acknowledge form:\n%s", before)
	}
	if strings.Contains(before, `badge acknowledged`) {
		t.Errorf("overview marks a problem nobody acknowledged:\n%s", before)
	}

	rec := e.do(http.MethodPost, acknowledgePath(id), url.Values{}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if at := e.openRecord("a").AcknowledgedAt; at == nil || !at.Equal(testNow) {
		t.Errorf("AcknowledgedAt = %v, want %v", at, testNow)
	}

	after := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if !strings.Contains(after, `badge acknowledged`) || !strings.Contains(after, "2026-11-03 12:00:00 UTC") {
		t.Errorf("overview does not show the acknowledgement with its time:\n%s", after)
	}
	if !strings.Contains(after, "1 current problem") {
		t.Errorf("an acknowledged problem must stay on the overview:\n%s", after)
	}
	if strings.Contains(after, acknowledgePath(id)) {
		t.Errorf("overview still offers to acknowledge:\n%s", after)
	}
}

func TestWarningCannotBeAcknowledged(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	id := e.firingID("a", checks.Warning)

	body := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if strings.Contains(body, acknowledgePath(id)) {
		t.Errorf("overview offers to acknowledge a warning:\n%s", body)
	}

	rec := e.do(http.MethodPost, acknowledgePath(id), url.Values{}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if at := e.openRecord("a").AcknowledgedAt; at != nil {
		t.Errorf("AcknowledgedAt = %v, want nil for a warning", at)
	}
}

func TestAcknowledgeUnknownProblemReturnsToOverview(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	if rec := e.do(http.MethodPost, acknowledgePath(42), url.Values{}, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

func TestAcknowledgeInvalidID(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	if rec := e.do(http.MethodPost, "/problems/abc/acknowledge", url.Values{}, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAcknowledgeNeedsLoginAndPost(t *testing.T) {
	e := newTestEnv(t)
	id := e.firingID("a", checks.Critical)

	rec := e.do(http.MethodPost, acknowledgePath(id), url.Values{}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("without login: status = %d, location = %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do(http.MethodGet, acknowledgePath(id), nil, e.login()); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
	if at := e.openRecord("a").AcknowledgedAt; at != nil {
		t.Errorf("AcknowledgedAt = %v, want nil", at)
	}
}

func TestAcknowledgeCrossOriginProtection(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		want   int
	}{
		{"same origin", "Sec-Fetch-Site", "same-origin", http.StatusSeeOther},
		{"other site", "Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		// SameSite=Lax would let this one through with the cookie.
		{"other host of the same site", "Sec-Fetch-Site", "same-site", http.StatusForbidden},
		// Browsers on plain HTTP send no Sec-Fetch-Site, only Origin.
		{"origin matches host", "Origin", "http://example.com", http.StatusSeeOther},
		{"origin is another host", "Origin", "https://evil.example", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			cookie := e.login()
			id := e.firingID("a", checks.Critical)

			req := httptest.NewRequest(http.MethodPost, acknowledgePath(id), nil) // Host is example.com
			req.Header.Set(tt.header, tt.value)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			acknowledged := e.openRecord("a").AcknowledgedAt != nil
			if acknowledged != (tt.want == http.StatusSeeOther) {
				t.Errorf("acknowledged = %v after status %d", acknowledged, rec.Code)
			}
		})
	}
}

func TestAcknowledgeIsExplainedOnceAndOnlyIfItCanBeUsed(t *testing.T) {
	const hint = "Acknowledge stops the reminders"

	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	e.firingID("warning", checks.Warning)
	if body := e.do(http.MethodGet, "/", nil, cookie).Body.String(); strings.Contains(body, hint) {
		t.Errorf("hint shown although nothing can be acknowledged:\n%s", body)
	}

	e.firingID("one", checks.Critical)
	e.firingID("two", checks.Critical)
	if body := e.do(http.MethodGet, "/", nil, cookie).Body.String(); strings.Count(body, hint) != 1 {
		t.Errorf("hint shown %d times, want once:\n%s", strings.Count(body, hint), body)
	}
}

func unacknowledgePath(id int64) string {
	return "/problems/" + strconv.FormatInt(id, 10) + "/unacknowledge"
}

func TestRemoveAcknowledgementFromOverview(t *testing.T) {
	const hint = "Removing an acknowledgement brings the reminders back"

	e := newTestEnv(t)
	cookie := e.login()
	e.status.LastRun = testNow
	id := e.firingID("a", checks.Critical)

	before := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if strings.Contains(before, unacknowledgePath(id)) || strings.Contains(before, hint) {
		t.Errorf("overview offers to remove an acknowledgement nobody made:\n%s", before)
	}

	e.do(http.MethodPost, acknowledgePath(id), url.Values{}, cookie)
	e.do(http.MethodPost, acknowledgePath(e.firingID("b", checks.Critical)), url.Values{}, cookie)
	acknowledged := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if !strings.Contains(acknowledged, `action="`+unacknowledgePath(id)+`"`) {
		t.Errorf("overview has no form to remove the acknowledgement:\n%s", acknowledged)
	}
	if strings.Count(acknowledged, hint) != 1 {
		t.Errorf("hint shown %d times, want once:\n%s", strings.Count(acknowledged, hint), acknowledged)
	}

	rec := e.do(http.MethodPost, unacknowledgePath(id), url.Values{}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if at := e.openRecord("a").AcknowledgedAt; at != nil {
		t.Errorf("AcknowledgedAt = %v, want nil", at)
	}
	if at := e.openRecord("b").AcknowledgedAt; at == nil {
		t.Error("the acknowledgement of another problem was removed")
	}

	after := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if !strings.Contains(after, `action="`+acknowledgePath(id)+`"`) || strings.Contains(after, unacknowledgePath(id)) {
		t.Errorf("overview does not offer to acknowledge again:\n%s", after)
	}
}

func TestRemoveAcknowledgementNeedsLoginPostAndValidID(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login()
	id := e.firingID("a", checks.Critical)
	e.do(http.MethodPost, acknowledgePath(id), url.Values{}, cookie)

	rec := e.do(http.MethodPost, unacknowledgePath(id), url.Values{}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("without login: status = %d, location = %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do(http.MethodGet, unacknowledgePath(id), nil, cookie); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/problems/abc/unacknowledge", url.Values{}, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status = %d, want 400", rec.Code)
	}
	if rec := e.do(http.MethodPost, unacknowledgePath(42), url.Values{}, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("unknown id: status = %d, want 303", rec.Code)
	}
	if e.openRecord("a").AcknowledgedAt == nil {
		t.Error("the acknowledgement was removed")
	}
}

func TestRemoveAcknowledgementRefusesOtherOrigins(t *testing.T) {
	for _, value := range []string{"cross-site", "same-site"} {
		t.Run(value, func(t *testing.T) {
			e := newTestEnv(t)
			cookie := e.login()
			id := e.firingID("a", checks.Critical)
			e.do(http.MethodPost, acknowledgePath(id), url.Values{}, cookie)

			req := httptest.NewRequest(http.MethodPost, unacknowledgePath(id), nil)
			req.Header.Set("Sec-Fetch-Site", value)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if e.openRecord("a").AcknowledgedAt == nil {
				t.Error("the acknowledgement was removed by a request from another origin")
			}
		})
	}
}
