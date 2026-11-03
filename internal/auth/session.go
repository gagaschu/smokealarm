package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
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
}

// Start logs a user in: it creates a session and sets the cookie. Every
// login method (the password form now, OIDC later) ends here.
func (s *Sessions) Start(w http.ResponseWriter, r *http.Request, username string) error {
	id, err := newSessionID()
	if err != nil {
		return err
	}
	now := s.Now()
	expires := now.Add(s.TTL)
	err = s.Store.CreateSession(r.Context(), store.Session{
		IDHash: hashID(id), Username: username, CreatedAt: now, ExpiresAt: expires,
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
	return session.Username, found, nil
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
