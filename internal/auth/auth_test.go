package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gagaschu/smokealarm/internal/store"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func TestHashPasswordAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash = %q, want a bcrypt hash", hash)
	}

	login := PasswordLogin{Username: "admin", Hash: []byte(hash)}
	tests := []struct {
		name, user, password string
		want                 bool
	}{
		{"right credentials", "admin", "correct horse", true},
		{"wrong password", "admin", "wrong", false},
		{"wrong user", "root", "correct horse", false},
		{"both wrong", "root", "wrong", false},
		{"empty", "", "", false},
		{"user is case sensitive", "Admin", "correct horse", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := login.Verify(tt.user, tt.password); got != tt.want {
				t.Errorf("Verify(%q, %q) = %v, want %v", tt.user, tt.password, got, tt.want)
			}
		})
	}
}

func TestHashPasswordRejectsTooLongPassword(t *testing.T) {
	// bcrypt only uses the first 72 bytes; silently ignoring the rest would
	// make a long password weaker than it looks, so this is an error.
	if _, err := HashPassword(strings.Repeat("x", 73)); err == nil {
		t.Error("expected an error for a password longer than 72 bytes")
	}
}

func TestVerifyWithBrokenHash(t *testing.T) {
	login := PasswordLogin{Username: "admin", Hash: []byte("not a hash")}
	if login.Verify("admin", "anything") {
		t.Error("a broken hash must never verify")
	}
}

func newTestSessions(t *testing.T, now *time.Time, secure bool) (*Sessions, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"), func() time.Time { return *now })
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Sessions{Store: db, TTL: time.Hour, SecureCookie: secure, Now: func() time.Time { return *now }}, db
}

// login starts a session and returns the cookie the browser would get.
func login(t *testing.T, s *Sessions) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := s.Start(rec, httptest.NewRequest(http.MethodPost, "/login", nil), "admin", MethodPassword); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	return cookies[0]
}

func requestWith(cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestSessionRoundTrip(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	cookie := login(t, s)

	user, ok, err := s.Valid(requestWith(cookie))
	if err != nil || !ok || user != "admin" {
		t.Fatalf("Valid = %q, %v, %v; want admin, true", user, ok, err)
	}
	if _, ok, _ := s.Valid(requestWith(nil)); ok {
		t.Error("a request without a cookie must not be logged in")
	}
	if _, ok, _ := s.Valid(requestWith(&http.Cookie{Name: cookieName, Value: "guessed"})); ok {
		t.Error("a made-up session id must not be accepted")
	}
}

func TestSessionExpires(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	cookie := login(t, s)

	now = testNow.Add(59 * time.Minute)
	if _, ok, _ := s.Valid(requestWith(cookie)); !ok {
		t.Error("session invalid before its TTL ran out")
	}
	now = testNow.Add(time.Hour)
	if _, ok, _ := s.Valid(requestWith(cookie)); ok {
		t.Error("session still valid after its TTL")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	cookie := login(t, s)

	rec := httptest.NewRecorder()
	if err := s.End(rec, requestWith(cookie)); err != nil {
		t.Fatalf("End: %v", err)
	}
	if _, ok, _ := s.Valid(requestWith(cookie)); ok {
		t.Error("session still valid after logout, even though the browser might keep the cookie")
	}
	cleared := rec.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Value != "" || cleared[0].Expires.After(testNow) {
		t.Errorf("logout must expire the cookie, got %+v", cleared)
	}

	// Logging out without a session must not fail.
	if err := s.End(httptest.NewRecorder(), requestWith(nil)); err != nil {
		t.Errorf("End without a cookie: %v", err)
	}
}

func TestSessionIDIsNotStored(t *testing.T) {
	now := testNow
	s, db := newTestSessions(t, &now, true)
	cookie := login(t, s)

	// Looking up the raw cookie value must find nothing; only its hash is stored.
	if _, found, _ := db.Session(context.Background(), cookie.Value, now); found {
		t.Error("the raw session id is stored in the database")
	}
	if _, found, _ := db.Session(context.Background(), hashID(cookie.Value), now); !found {
		t.Error("the hash of the session id is not stored")
	}
	if len(cookie.Value) < 40 {
		t.Errorf("session id %q is too short to be unguessable", cookie.Value)
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	first, second := login(t, s), login(t, s)
	if first.Value == second.Value {
		t.Error("two logins got the same session id")
	}
}

func TestCookieAttributes(t *testing.T) {
	for _, secure := range []bool{true, false} {
		now := testNow
		s, _ := newTestSessions(t, &now, secure)
		cookie := login(t, s)

		if cookie.Name != "smokealarm_session" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
			t.Errorf("cookie = %+v, want HttpOnly, SameSite=Lax, Path=/", cookie)
		}
		if cookie.Secure != secure {
			t.Errorf("Secure = %v, want %v", cookie.Secure, secure)
		}
		if !cookie.Expires.Equal(testNow.Add(time.Hour)) {
			t.Errorf("Expires = %v, want %v", cookie.Expires, testNow.Add(time.Hour))
		}
	}
}

func TestRequire(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	reached := false
	protected := s.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, requestWith(nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" || reached {
		t.Errorf("without a session: status %d, location %q, reached %v; want redirect to /login",
			rec.Code, rec.Header().Get("Location"), reached)
	}

	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, requestWith(login(t, s)))
	if rec.Code != http.StatusOK || !reached {
		t.Errorf("with a session: status %d, reached %v; want the handler to run", rec.Code, reached)
	}
}

func TestRateLimiter(t *testing.T) {
	now := testNow
	limiter := &RateLimiter{Now: func() time.Time { return now }}
	const client = "10.0.0.1"

	for i := range 5 {
		if !limiter.Attempt(client) {
			t.Fatalf("try %d was refused, want to allow 5 tries", i+1)
		}
	}
	if limiter.Attempt(client) {
		t.Error("the sixth try was allowed")
	}
	if !limiter.Attempt("10.0.0.2") {
		t.Error("another client must not be affected")
	}

	// A refused try does not count, so this one must not extend the block.
	now = testNow.Add(14 * time.Minute)
	if limiter.Attempt(client) {
		t.Error("allowed again before the window ended")
	}
	now = testNow.Add(15*time.Minute + time.Second)
	if !limiter.Attempt(client) {
		t.Error("still blocked after the window ended")
	}
}

func TestRateLimiterResetOnSuccess(t *testing.T) {
	limiter := &RateLimiter{Now: func() time.Time { return testNow }}
	for range 5 {
		limiter.Attempt("c")
	}
	limiter.Succeeded("c")
	if !limiter.Attempt("c") {
		t.Error("a successful login must reset the counter")
	}
}

func TestBcryptCostIsReasonable(t *testing.T) {
	hash, err := HashPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil || cost < 10 {
		t.Errorf("cost = %d, %v; want at least 10", cost, err)
	}
}

// A changed password or admin user must end the sessions from before, or an
// attacker stays logged in until the session expires.
func TestSessionEndsWhenTheCredentialsChange(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s, _ := newTestSessions(t, &now, true)
	s.Credentials = CredentialsFingerprint("admin", "old-hash")
	cookie := login(t, s)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	if _, ok, err := s.Valid(req); err != nil || !ok {
		t.Fatalf("Valid before the change = %v, %v", ok, err)
	}

	for _, changed := range []string{
		CredentialsFingerprint("admin", "new-hash"),
		CredentialsFingerprint("other", "old-hash"),
	} {
		s.Credentials = changed
		if _, ok, err := s.Valid(req); err != nil || ok {
			t.Errorf("Valid after the change = %v, %v; want no valid session", ok, err)
		}
	}
}

func TestCredentialsFingerprintDoesNotMixUpItsParts(t *testing.T) {
	if CredentialsFingerprint("ab", "c") == CredentialsFingerprint("a", "bc") {
		t.Error("different credentials must have different fingerprints")
	}
}

// Clients that never come back must not stay in the map.
func TestRateLimiterForgetsOldClients(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	l := &RateLimiter{Now: func() time.Time { return now }}
	for i := range 100 {
		l.Attempt(fmt.Sprintf("10.0.0.%d", i))
	}

	now = now.Add(attemptWindow + time.Second)
	l.Attempt("10.0.1.1")

	if len(l.attempts) != 1 {
		t.Errorf("map holds %d clients, want only the recent one", len(l.attempts))
	}
}

// loginWith starts a session with the given method and returns its cookie.
func loginWith(t *testing.T, s *Sessions, method Method) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := s.Start(rec, httptest.NewRequest(http.MethodGet, "/login/oidc/callback", nil), "gabriel@example.org", method); err != nil {
		t.Fatalf("Start(%s): %v", method, err)
	}
	return rec.Result().Cookies()[0]
}

func TestOIDCSessionIsValidWhileOIDCIsOn(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	s.Credentials = CredentialsFingerprint("admin", "hash")
	s.OIDCCredentials = OIDCFingerprint("https://id.example.org", "smokealarm", []string{"a@example.org"}, nil)

	oidcCookie := loginWith(t, s, MethodOIDC)
	passwordCookie := loginWith(t, s, MethodPassword)

	user, ok, err := s.Valid(requestWith(oidcCookie))
	if err != nil || !ok || user != "gabriel@example.org" {
		t.Fatalf("OIDC session: Valid = %q, %v, %v; want the user, true", user, ok, err)
	}
	if _, ok, _ := s.Valid(requestWith(passwordCookie)); !ok {
		t.Error("the password session must stay valid next to OIDC")
	}

	// A new allowlist ends the OIDC sessions and leaves the password ones.
	s.OIDCCredentials = OIDCFingerprint("https://id.example.org", "smokealarm", []string{"someone-else@example.org"}, nil)
	if _, ok, _ := s.Valid(requestWith(oidcCookie)); ok {
		t.Error("an OIDC session must end when the allowlist changes")
	}
	if _, ok, _ := s.Valid(requestWith(passwordCookie)); !ok {
		t.Error("the password session must not end because of the OIDC allowlist")
	}

	// Switching OIDC off ends its sessions too.
	s.OIDCCredentials = ""
	if _, ok, _ := s.Valid(requestWith(oidcCookie)); ok {
		t.Error("an OIDC session must end when OIDC is switched off")
	}
}

func TestPasswordChangeLeavesOIDCSessionsAlone(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	s.Credentials = CredentialsFingerprint("admin", "old hash")
	s.OIDCCredentials = OIDCFingerprint("https://id.example.org", "smokealarm", nil, []string{"ops"})
	oidcCookie := loginWith(t, s, MethodOIDC)
	passwordCookie := loginWith(t, s, MethodPassword)

	s.Credentials = CredentialsFingerprint("admin", "new hash")
	if _, ok, _ := s.Valid(requestWith(passwordCookie)); ok {
		t.Error("the password session must end when the password changes")
	}
	if _, ok, _ := s.Valid(requestWith(oidcCookie)); !ok {
		t.Error("the OIDC session does not depend on the password")
	}
}

func TestSessionWithoutFingerprintNeverMatchesOIDC(t *testing.T) {
	// Sessions from before the fingerprint existed have an empty value. An
	// empty OIDCCredentials (OIDC off) must not accept them as OIDC sessions.
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	cookie := loginWith(t, s, MethodPassword) // stored with the empty password fingerprint
	s.Credentials = CredentialsFingerprint("admin", "hash")
	s.OIDCCredentials = ""
	if _, ok, _ := s.Valid(requestWith(cookie)); ok {
		t.Error("a session with an empty fingerprint must not be valid")
	}
}

func TestStartRefusesOIDCWhenItIsNotConfigured(t *testing.T) {
	now := testNow
	s, _ := newTestSessions(t, &now, true)
	rec := httptest.NewRecorder()
	err := s.Start(rec, httptest.NewRequest(http.MethodGet, "/", nil), "x", MethodOIDC)
	if err == nil {
		t.Fatal("expected an error: OIDC is not configured")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("no cookie must be set when the session could not be started")
	}
	if err := s.Start(rec, httptest.NewRequest(http.MethodGet, "/", nil), "x", Method("magic")); err == nil {
		t.Error("expected an error for an unknown login method")
	}
}

func TestOIDCFingerprint(t *testing.T) {
	base := OIDCFingerprint("https://id.example.org", "smokealarm", []string{"a@example.org", "b@example.org"}, []string{"ops", "dev"})
	if got := OIDCFingerprint("https://id.example.org", "smokealarm", []string{"b@example.org", "a@example.org"}, []string{"dev", "ops"}); got != base {
		t.Error("a different order of the allowlists must not change the fingerprint")
	}
	tests := []struct {
		name             string
		issuer, clientID string
		emails, groups   []string
	}{
		{"other issuer", "https://other.example.org", "smokealarm", []string{"a@example.org", "b@example.org"}, []string{"ops", "dev"}},
		{"other client", "https://id.example.org", "other", []string{"a@example.org", "b@example.org"}, []string{"ops", "dev"}},
		{"fewer emails", "https://id.example.org", "smokealarm", []string{"a@example.org"}, []string{"ops", "dev"}},
		{"fewer groups", "https://id.example.org", "smokealarm", []string{"a@example.org", "b@example.org"}, []string{"ops"}},
		{"an email moved to the groups", "https://id.example.org", "smokealarm", []string{"a@example.org"}, []string{"b@example.org", "ops", "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OIDCFingerprint(tt.issuer, tt.clientID, tt.emails, tt.groups)
			if got == base {
				t.Error("fingerprint did not change")
			}
			if got == "" {
				t.Error("fingerprint is empty, which means OIDC is off")
			}
		})
	}
	if base == CredentialsFingerprint("https://id.example.org", "smokealarm") {
		t.Error("an OIDC fingerprint must never equal a password fingerprint")
	}
}
