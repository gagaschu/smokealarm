package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gagaschu/smokealarm/internal/store"
)

// cookieName is the name of the session cookie.
const cookieName = "smokealarm_session"

// Sessions creates and checks login sessions. They live in the database, so
// they survive restarts. The browser only holds a random id; the database
// only holds the hash of that id.
type Sessions struct {
	Store        *store.Store
	TTL          time.Duration
	SecureCookie bool
	Now          func() time.Time
	// Credentials identifies the current login data (see
	// CredentialsFingerprint). A session started with other data is not valid,
	// so changing the password also throws out whoever had the old one.
	Credentials string
	// OIDCCredentials is the same for sessions that were started by the OIDC
	// login (see OIDCFingerprint). Empty means OIDC is off, and then no OIDC
	// session is valid.
	OIDCCredentials string
}

// Method says how a user logged in. It decides which fingerprint a session
// carries.
type Method string

// The ways to log in.
const (
	MethodPassword Method = "password"
	MethodOIDC     Method = "oidc"
)

// CredentialsFingerprint turns the admin user and the password hash into the
// value for Sessions.Credentials. The zero byte keeps the two parts apart.
func CredentialsFingerprint(username, passwordHash string) string {
	return hashID(username + "\x00" + passwordHash)
}

// OIDCFingerprint turns the settings that decide who may log in with OIDC
// into the value for Sessions.OIDCCredentials. Changing the provider, the
// client or an allowlist therefore ends the OIDC sessions, like a new
// password ends the password sessions. The lists are sorted, so only a real
// change counts, not a different order.
func OIDCFingerprint(issuer, clientID string, emails, groups []string) string {
	sorted := func(items []string) string {
		items = slices.Clone(items)
		slices.Sort(items)
		return strings.Join(items, "\x01")
	}
	return hashID("oidc\x00" + issuer + "\x00" + clientID + "\x00" + sorted(emails) + "\x00" + sorted(groups))
}

// fingerprint returns the value a session of this login method carries.
func (s *Sessions) fingerprint(method Method) (string, error) {
	switch method {
	case MethodPassword:
		return s.Credentials, nil
	case MethodOIDC:
		if s.OIDCCredentials == "" {
			return "", errors.New("OIDC login is not configured")
		}
		return s.OIDCCredentials, nil
	}
	return "", fmt.Errorf("unknown login method %q", method)
}

// Start logs a user in: it creates a session and sets the cookie. Every
// login method (the password form and OIDC) ends here.
func (s *Sessions) Start(w http.ResponseWriter, r *http.Request, username string, method Method) error {
	credentials, err := s.fingerprint(method)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	id, err := newSessionID()
	if err != nil {
		return err
	}
	now := s.Now()
	expires := now.Add(s.TTL)
	err = s.Store.CreateSession(r.Context(), store.Session{
		IDHash: hashID(id), Username: username, CreatedAt: now, ExpiresAt: expires,
		CredentialsHash: credentials,
	})
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	http.SetCookie(w, s.cookie(id, expires))
	return nil
}

// End logs the user out: it deletes the session and expires the cookie.
func (s *Sessions) End(w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie(cookieName); err == nil {
		if err := s.Store.DeleteSession(r.Context(), hashID(cookie.Value)); err != nil {
			return fmt.Errorf("end session: %w", err)
		}
	}
	// An expiry in the past makes the browser drop the cookie.
	http.SetCookie(w, s.cookie("", time.Unix(0, 0)))
	return nil
}

// Valid reports whether the request carries a valid session, and for which
// user.
func (s *Sessions) Valid(r *http.Request) (string, bool, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false, nil // no cookie: not logged in, not an error
	}
	session, found, err := s.Store.Session(r.Context(), hashID(cookie.Value), s.Now())
	if err != nil {
		return "", false, fmt.Errorf("check session: %w", err)
	}
	if !found || !s.knowsCredentials(session.CredentialsHash) {
		return "", false, nil
	}
	return session.Username, true, nil
}

// knowsCredentials reports whether a session started with these credentials
// is still valid: the password ones, or the OIDC ones while OIDC is on. An
// empty OIDC value never matches, because old sessions have an empty one.
func (s *Sessions) knowsCredentials(hash string) bool {
	if hash == s.Credentials {
		return true
	}
	return s.OIDCCredentials != "" && hash == s.OIDCCredentials
}

// Require lets only requests with a valid session through; everybody else is
// sent to the login page. It does not care how the session was created.
func (s *Sessions) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok, err := s.Valid(r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Sessions) cookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,           // not readable by scripts
		Secure:   s.SecureCookie, // only over HTTPS; off for local development
		SameSite: http.SameSiteLaxMode,
	}
}

// newSessionID returns 32 random bytes, URL-safe encoded: unguessable.
func newSessionID() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// hashID is what the database stores instead of the session id.
func hashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
