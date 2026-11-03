// Package auth handles who may use the web UI: password check, sessions and
// the limit on failed logins. The session middleware only asks "is there a
// valid session?", so another way to log in (OIDC, later) can be added
// without touching it.
package auth

import (
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
