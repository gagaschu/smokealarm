// Package auth handles who may use the web UI: the password check, the OIDC
// login, sessions and the limit on failed logins. The session middleware only
// asks "is there a valid session?", so a way to log in can be added without
// touching it: the password form and OIDC both end in Sessions.Start.
package auth

import (
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns the bcrypt hash of a password, for use in
// SMOKEALARM_ADMIN_PASSWORD_HASH.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// PasswordLogin checks the credentials of the single local admin user.
type PasswordLogin struct {
	Username string
	Hash     []byte // bcrypt hash of the password
}

// Verify reports whether the credentials are correct. It always does the
// expensive bcrypt comparison, even for a wrong username, so the response
// time does not reveal whether the username exists.
func (p PasswordLogin) Verify(username, password string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(p.Username)) == 1
	passwordOK := bcrypt.CompareHashAndPassword(p.Hash, []byte(password)) == nil
	return userOK && passwordOK
}
