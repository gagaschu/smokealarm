package auth

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// oidcCookieName holds state, nonce and PKCE verifier of a login that is
	// under way. They are random, belong to one browser and are used once.
	oidcCookieName = "smokealarm_oidc"
	// oidcCookiePath limits the cookie to the two OIDC routes.
	oidcCookiePath = "/login/oidc"
	// oidcLoginTimeout is how long the user has to log in at the provider.
	oidcLoginTimeout = 10 * time.Minute
	// oidcRetryAfter is how long a failed discovery is not tried again, so
	// that nobody can make smokealarm hammer an identity provider that is down.
	oidcRetryAfter = 30 * time.Second
	// oidcRequestTimeout bounds every request to the provider.
	oidcRequestTimeout = 10 * time.Second
	// minSecretPart is the shortest text that is taken out of an error as a
	// possible secret; shorter ones are words, and removing them would make
	// the message unreadable.
	minSecretPart = 8
)

// OIDC is the login with an OpenID Connect identity provider: the
// authorization code flow with PKCE. Begin sends the user to the provider,
// Finish checks what comes back and decides whether the user may in. The
// caller then starts a session with Sessions.Start.
//
// The provider is contacted at the first login, not at startup, so smokealarm
// does not depend on it to run.
type OIDC struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string // BASE_URL + /login/oidc/callback, as registered at the provider
	Scopes       []string
	GroupsClaim  string
	// AllowedEmails (lower case) and AllowedGroups are the allowlist: one
	// match is enough.
	AllowedEmails []string
	AllowedGroups []string
	SecureCookie  bool
	Client        *http.Client // see NewOIDCHTTPClient
	Now           func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	failure  error     // why the last discovery failed
	failedAt time.Time // when, to wait before the next try
}

// NewOIDCHTTPClient returns the client for all requests to the identity
// provider. With a CA file it trusts that CA instead of the system's, for a
// provider with an internal certificate. It does not follow redirects, because
// a token request that is forwarded would send the client secret elsewhere.
// The proxy from HTTPS_PROXY is used.
func NewOIDCHTTPClient(caFile string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: oidcRequestTimeout,
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read SMOKEALARM_OIDC_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("SMOKEALARM_OIDC_CA_FILE: %s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   oidcRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// Begin starts a login: it remembers state, nonce and PKCE verifier in a
// short-lived cookie and returns the address of the provider to send the
// browser to.
func (o *OIDC) Begin(w http.ResponseWriter, r *http.Request) (string, error) {
	provider, err := o.discover(r.Context())
	if err != nil {
		return "", err
	}
	state, err := newSessionID()
	if err != nil {
		return "", err
	}
	nonce, err := newSessionID()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()

	// All three are URL-safe base64, so a dot cannot be part of one of them.
	http.SetCookie(w, o.cookie(state+"."+nonce+"."+verifier, int(oidcLoginTimeout/time.Second)))
	authURL := o.oauth(provider).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	return authURL, nil
}

// Finish ends a login: it checks that this browser started it, exchanges the
// code for tokens, checks the ID token and applies the allowlist. It returns
// the name to show for the user. The error says why a login failed; it is for
// the log, not for the user, and never contains a secret.
func (o *OIDC) Finish(w http.ResponseWriter, r *http.Request) (string, error) {
	cookie, cookieErr := r.Cookie(oidcCookieName)
	// One try per cookie, whatever comes out of it.
	http.SetCookie(w, o.cookie("", -1))

	query := r.URL.Query()
	if problem := query.Get("error"); problem != "" {
		return "", fmt.Errorf("the provider refused the login: %.100q", problem)
	}
	if cookieErr != nil {
		return "", errors.New("no login is under way: the cookie is missing or expired")
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return "", errors.New("the login cookie is not valid")
	}
	state, nonce, verifier := parts[0], parts[1], parts[2]
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
		return "", errors.New("the state does not match the one this browser started with")
	}
	code := query.Get("code")
	if code == "" {
		return "", errors.New("the provider sent no code")
	}

	provider, err := o.discover(r.Context())
	if err != nil {
		return "", err
	}
	ctx := oidc.ClientContext(r.Context(), o.Client)
	token, err := o.oauth(provider).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", fmt.Errorf("exchange the code for tokens: %w", o.withoutSecrets(err, code))
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return "", errors.New("the provider sent no ID token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: o.ClientID, Now: o.Now}).Verify(ctx, rawIDToken)
	if err != nil {
		return "", fmt.Errorf("check the ID token: %w", o.withoutSecrets(err, code))
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return "", errors.New("the nonce of the ID token does not match the one this browser started with")
	}

	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return "", fmt.Errorf("read the claims of the ID token: %w", err)
	}
	return o.allow(claims)
}

// discover returns the provider, asking for its configuration if that has not
// worked yet. A failure is remembered for a short time, so a provider that is
// down is not asked again and again.
func (o *OIDC) discover(ctx context.Context) (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return o.provider, nil
	}
	if o.failure != nil && o.Now().Sub(o.failedAt) < oidcRetryAfter {
		return nil, o.failure
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, o.Client), o.IssuerURL)
	if err != nil {
		o.failure = fmt.Errorf("discover the OIDC provider %s: %w", o.IssuerURL, err)
		o.failedAt = o.Now()
		return nil, o.failure
	}
	o.provider, o.failure = provider, nil
	return provider, nil
}

func (o *OIDC) oauth(provider *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  o.RedirectURL,
		Scopes:       o.Scopes,
	}
}

// cookie builds the login cookie. A negative maxAge deletes it.
func (o *OIDC) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     oidcCookieName,
		Value:    value,
		Path:     oidcCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   o.SecureCookie,
		// Lax, because the provider sends the browser back with a GET: the
		// cookie must come along on that first request.
		SameSite: http.SameSiteLaxMode,
	}
}

// allow applies the allowlist to the claims of a verified ID token and returns
// the name to show for the user.
//
// An email counts only if the provider says it is verified: otherwise anybody
// who may enter any address at the provider could claim an allowed one. The
// groups come from the ID token only; the userinfo endpoint is not asked.
func (o *OIDC) allow(claims map[string]json.RawMessage) (string, error) {
	email := strings.ToLower(stringClaim(claims, "email"))
	verified := email != "" && boolClaim(claims, "email_verified")

	name := stringClaim(claims, "preferred_username")
	if verified {
		name = email
	}
	if name == "" {
		name = stringClaim(claims, "sub")
	}

	if verified && slices.Contains(o.AllowedEmails, email) {
		return name, nil
	}
	for _, group := range listClaim(claims, o.GroupsClaim) {
		if slices.Contains(o.AllowedGroups, group) {
			return name, nil
		}
	}

	reason := "no allowed email or group"
	if email != "" && !verified && slices.Contains(o.AllowedEmails, email) {
		reason = "the email is on the allowlist but the provider does not say it is verified"
	}
	return "", fmt.Errorf("%.100q is not allowed: %s", name, reason)
}

// withoutSecrets takes the client secret and the code out of an error from the
// token request. The libraries do not put them there, but a provider can echo
// them in an error body, and an error ends up in the log.
func (o *OIDC) withoutSecrets(err error, code string) error {
	text := err.Error()
	for _, secret := range []string{o.ClientSecret, code} {
		if len(secret) >= minSecretPart {
			text = strings.ReplaceAll(text, secret, "[removed]")
		}
	}
	return errors.New(text)
}

// stringClaim returns a claim that is a string, or "" if it is missing or of
// another type.
func stringClaim(claims map[string]json.RawMessage, name string) string {
	var value string
	if raw, ok := claims[name]; ok && json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

// boolClaim reads a claim that is true. Some providers (Amazon Cognito) send
// "true" as a string instead of a boolean.
func boolClaim(claims map[string]json.RawMessage, name string) bool {
	var value bool
	if raw, ok := claims[name]; ok && json.Unmarshal(raw, &value) == nil {
		return value
	}
	return stringClaim(claims, name) == "true"
}

// listClaim returns a claim that is a list of strings, or one string.
func listClaim(claims map[string]json.RawMessage, name string) []string {
	var values []string
	if raw, ok := claims[name]; ok && json.Unmarshal(raw, &values) == nil {
		return values
	}
	if single := stringClaim(claims, name); single != "" {
		return []string{single}
	}
	return nil
}
