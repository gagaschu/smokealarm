package config

import "golang.org/x/crypto/bcrypt"

// validatePasswordHash makes sure the value is a bcrypt hash, so a typo (or a
// plain password by mistake) fails at startup and not at the first login.
// The value itself is never put into the message.
func validatePasswordHash(r *reader, hash string) {
	if hash == "" {
		return // already reported as missing
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		r.fail("SMOKEALARM_ADMIN_PASSWORD_HASH: not a valid bcrypt hash (create one with \"smokealarm hash-password\"; in shells and YAML, quote the value because it contains $ characters)")
	}
}
