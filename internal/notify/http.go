package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// httpTimeout limits one request to a notification target, so a service that
// hangs cannot block the main loop for long. It is a real network deadline, so
// it uses the real clock.
const httpTimeout = 15 * time.Second

// maxErrorBody is how much of a response body goes into an error message.
const maxErrorBody = 200

// maxErrorRead is how much of a response body is read for that excerpt. It is
// far more than the excerpt, because secrets are removed before the cut: a
// secret that the cut went through would no longer be recognized.
const maxErrorRead = 64 << 10

// minSecretPart is the shortest part of a URL path or query that counts as a
// possible secret. Shorter ones are words like "api" or "v1", and replacing
// those would make an error unreadable; a token is longer.
const minSecretPart = 8

// NewHTTPClient returns the client that all HTTP targets share. main creates
// it once. It does not follow redirects: a webhook that answers with one is
// misconfigured, and following it would send the message (and an
// Authorization header) somewhere else. The proxy from HTTPS_PROXY is used.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// request is one POST to a notification target.
type request struct {
	target      string // safe name for error messages, e.g. "slack"
	url         string // may contain a secret, so it never goes into an error
	contentType string
	body        []byte
	headers     map[string]string // values may be secrets
	secrets     []string          // removed from anything that goes into an error; the URL is added by post
}

// post sends the request with the context of the run. Any answer other than
// 2xx is an error with the status code and the start of the response body.
// Errors from net/http contain the request URL, which can be a secret (the
// Telegram token, a webhook URL), so the URL is taken out before wrapping.
func post(ctx context.Context, client *http.Client, r request) error {
	r.secrets = append(r.secrets, urlSecrets(r.url)...)
	// Longest first: a short secret inside a longer one must not break it up.
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(r.body))
	if err != nil {
		return fmt.Errorf("%s: create request: %w", r.target, withoutURL(err, r.secrets))
	}
	req.Header.Set("Content-Type", r.contentType)
	req.Header.Set("User-Agent", "smokealarm")
	for name, value := range r.headers {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: send request: %w", r.target, withoutURL(err, r.secrets))
	}
	defer func() { _ = resp.Body.Close() }() // nothing useful can be done with a close error

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorRead)) // a read error leaves the answer short
		excerpt := cutText(redact(strings.TrimSpace(string(answer)), r.secrets), maxErrorBody)
		return fmt.Errorf("%s: answered %s: %s", r.target, resp.Status, excerpt)
	}
	// Read the rest, so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// withoutURL returns the cause of a *url.Error without the URL around it. If
// the cause still contains a secret, it is replaced by a plain error with the
// secret taken out; that is the only case in which the chain is cut.
func withoutURL(err error, secrets []string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	text := err.Error()
	if redacted := redact(text, secrets); redacted != text {
		return errors.New(redacted)
	}
	return err
}

// redact replaces every secret in s by "<redacted>".
func redact(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	return s
}

// urlSecrets returns the parts of a URL that may be secret, because a server or
// net/http may repeat them: the whole URL, its host (a target is shown by its
// name only), a password, the path and the query, and also every single path
// segment and query value, as a server may quote just the token.
func urlSecrets(rawURL string) []string {
	secrets := []string{rawURL}
	u, err := url.Parse(rawURL)
	if err != nil {
		return secrets
	}
	secrets = append(secrets, u.Hostname())
	if password, set := u.User.Password(); set {
		secrets = append(secrets, password)
	}
	if u.Path != "/" {
		secrets = append(secrets, u.Path, u.EscapedPath())
	}
	secrets = append(secrets, u.RawQuery)

	isSlash := func(r rune) bool { return r == '/' }
	isQuerySeparator := func(r rune) bool { return r == '&' || r == '=' }
	parts := strings.FieldsFunc(u.Path, isSlash)
	parts = append(parts, strings.FieldsFunc(u.EscapedPath(), isSlash)...)
	parts = append(parts, strings.FieldsFunc(u.RawQuery, isQuerySeparator)...)
	for _, values := range u.Query() {
		parts = append(parts, values...)
	}
	for _, part := range parts {
		if len(part) >= minSecretPart {
			secrets = append(secrets, part)
		}
	}
	return secrets
}
