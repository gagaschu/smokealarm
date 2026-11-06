package config

import (
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// OIDCConfig holds the settings of the OIDC login. It is a second way to log
// in next to the local admin, which stays as the way in when the identity
// provider is down. OIDC is on when the issuer URL is set.
//
// The client secret is a secret: error messages name the variable, never its
// value.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// Name is shown in the login button ("Login with <Name>").
	Name        string
	Scopes      []string
	GroupsClaim string // the claim of the ID token with the groups of the user
	// AllowedEmails and AllowedGroups are the allowlist: one match is enough.
	// Emails are lower case; groups are compared exactly as written.
	AllowedEmails []string
	AllowedGroups []string
	// CAFile holds the CA for the certificate of the identity provider, for
	// one with an internal certificate. Empty = the system roots.
	CAFile string
}

// Enabled reports whether the OIDC login is switched on.
func (c OIDCConfig) Enabled() bool {
	return c.IssuerURL != ""
}

// The variables of the OIDC login. They appear in several error messages, so
// they are named once here.
const (
	envOIDCIssuer        = "SMOKEALARM_OIDC_ISSUER_URL"
	envOIDCClientID      = "SMOKEALARM_OIDC_CLIENT_ID"
	envOIDCClientSecret  = "SMOKEALARM_OIDC_CLIENT_SECRET"
	envOIDCName          = "SMOKEALARM_OIDC_NAME"
	envOIDCScopes        = "SMOKEALARM_OIDC_SCOPES"
	envOIDCGroupsClaim   = "SMOKEALARM_OIDC_GROUPS_CLAIM"
	envOIDCAllowedEmails = "SMOKEALARM_OIDC_ALLOWED_EMAILS"
	envOIDCAllowedGroups = "SMOKEALARM_OIDC_ALLOWED_GROUPS"
	envOIDCCAFile        = "SMOKEALARM_OIDC_CA_FILE"
)

// oidcVariables lists every OIDC variable, to notice a setting that was made
// without the issuer URL.
func oidcVariables() []string {
	return []string{
		envOIDCIssuer, envOIDCClientID, envOIDCClientSecret, envOIDCName, envOIDCScopes,
		envOIDCGroupsClaim, envOIDCAllowedEmails, envOIDCAllowedGroups, envOIDCCAFile,
	}
}

// maxOIDCNameLength is how long the name in the login button may be.
const maxOIDCNameLength = 40

func loadOIDC(r *reader) OIDCConfig {
	c := OIDCConfig{
		IssuerURL:     strings.TrimSpace(r.text(envOIDCIssuer, "")),
		ClientID:      r.text(envOIDCClientID, ""),
		ClientSecret:  r.text(envOIDCClientSecret, ""),
		Name:          strings.TrimSpace(r.text(envOIDCName, "OIDC")),
		Scopes:        parseScopes(r.text(envOIDCScopes, "openid email")),
		GroupsClaim:   r.text(envOIDCGroupsClaim, "groups"),
		AllowedEmails: lowerAll(r.list(envOIDCAllowedEmails)),
		AllowedGroups: r.list(envOIDCAllowedGroups),
		CAFile:        r.text(envOIDCCAFile, ""),
	}
	// A setting without the issuer URL is most likely a typo or a forgotten
	// variable, and ignoring it would leave OIDC off without a word.
	if !c.Enabled() {
		for _, name := range oidcVariables() {
			if name != envOIDCIssuer && r.getenv(name) != "" {
				r.fail("%s is set, but %s is not: OIDC stays off. Set the issuer URL, or remove the OIDC variables", name, envOIDCIssuer)
				break
			}
		}
	}
	return c
}

// parseScopes splits a list that is separated by spaces, commas or both.
func parseScopes(s string) []string {
	return strings.FieldsFunc(s, func(c rune) bool { return c == ',' || unicode.IsSpace(c) })
}

func lowerAll(items []string) []string {
	for i, item := range items {
		items[i] = strings.ToLower(item)
	}
	return items
}

// validateOIDC checks the settings once OIDC is on. baseURL is needed for the
// redirect URI. No message contains the client secret.
func validateOIDC(r *reader, c OIDCConfig, baseURL string) {
	if !c.Enabled() {
		return
	}
	validateIssuerURL(r, c.IssuerURL)
	if c.ClientID == "" {
		r.fail("%s is required when %s is set", envOIDCClientID, envOIDCIssuer)
	}
	if c.ClientSecret == "" {
		r.fail("%s is required when %s is set", envOIDCClientSecret, envOIDCIssuer)
	}
	// Without a list nobody could log in, and a provider that knows many people
	// must not let all of them in, so there is no default.
	if len(c.AllowedEmails) == 0 && len(c.AllowedGroups) == 0 {
		r.fail("%s or %s is required when %s is set: without an allowlist nobody could log in with OIDC", envOIDCAllowedEmails, envOIDCAllowedGroups, envOIDCIssuer)
	}
	for _, email := range c.AllowedEmails {
		if !strings.Contains(email, "@") {
			r.fail("%s: %q is not an email address", envOIDCAllowedEmails, email)
		}
	}
	if baseURL == "" {
		r.fail("SMOKEALARM_BASE_URL is required when %s is set: the redirect URI of the provider is built from it", envOIDCIssuer)
	}
	if !slices.Contains(c.Scopes, "openid") {
		r.fail("%s: must contain \"openid\"", envOIDCScopes)
	}
	validateOIDCName(r, c.Name)
}

// validateIssuerURL wants https. Plain http is allowed for a provider on the
// same machine, for local development.
func validateIssuerURL(r *reader, value string) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		r.fail("%s: %q is not an http(s) URL", envOIDCIssuer, value)
		return
	}
	if u.RawQuery != "" || u.Fragment != "" {
		r.fail("%s: must not contain a query or a fragment", envOIDCIssuer)
	}
	if u.Scheme == "http" && !slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()) {
		r.fail("%s: must use https (plain http only for localhost)", envOIDCIssuer)
	}
}

// validateOIDCName keeps the button text short and on one line. The login page
// escapes it, so special characters are no risk.
func validateOIDCName(r *reader, name string) {
	if name == "" {
		r.fail("%s: must not be empty", envOIDCName)
	}
	if utf8.RuneCountInString(name) > maxOIDCNameLength {
		r.fail("%s: at most %d characters", envOIDCName, maxOIDCNameLength)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		r.fail("%s: must not contain control characters", envOIDCName)
	}
}
