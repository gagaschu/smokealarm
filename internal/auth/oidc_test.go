package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const (
	testClientID     = "smokealarm"
	testClientSecret = "client-secret-value-123"
	testRedirectURL  = "https://smokealarm.example.org/login/oidc/callback"
)

// fakeIdP is an identity provider in a test: discovery, the signing keys and
// the token endpoint, with a real RSA signature on the ID token. The
// authorization endpoint is not an HTTP endpoint: a test calls authorize with
// the address that Begin returned, as the browser would after the user logged
// in.
type fakeIdP struct {
	t       *testing.T
	server  *httptest.Server
	key     *rsa.PrivateKey
	signKey *rsa.PrivateKey // signs the tokens; a test can swap it for another key
	now     time.Time

	mu          sync.Mutex
	logins      map[string]fakeLogin // code -> what the user asked for
	override    map[string]any       // claims to change in the ID token; nil removes one
	down        bool                 // discovery answers 503
	discoveries int
	tokenError  string // the token endpoint fails with this description
}

type fakeLogin struct{ nonce, challenge string }

func newFakeIdP(t *testing.T, now time.Time) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIdP{t: t, key: key, signKey: key, now: now, logins: map[string]fakeLogin{}, override: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("/keys", f.keys)
	mux.HandleFunc("/token", f.token)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.discoveries++
	down := f.down
	f.mu.Unlock()
	if down {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                f.server.URL,
		"authorization_endpoint":                f.server.URL + "/authorize",
		"token_endpoint":                        f.server.URL + "/token",
		"jwks_uri":                              f.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *fakeIdP) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
		"n": b64(f.key.N.Bytes()),
		"e": b64(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

// token is the token endpoint: it checks the client login, the code, the
// redirect address and the PKCE verifier, and answers with a signed ID token.
func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// The client logs in with a header or with form fields; oauth2 tries one
	// and, if that fails, the other.
	user, password, ok := r.BasicAuth()
	if !ok {
		user, password = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if user != testClientID || password != testClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenError != "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": f.tokenError})
		return
	}
	login, found := f.logins[r.PostFormValue("code")]
	delete(f.logins, r.PostFormValue("code")) // a code works once
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if !found || r.PostFormValue("grant_type") != "authorization_code" ||
		r.PostFormValue("redirect_uri") != testRedirectURL || b64(sum[:]) != login.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}

	claims := map[string]any{
		"iss": f.server.URL, "aud": testClientID, "sub": "user-1", "nonce": login.nonce,
		"iat": f.now.Add(-time.Minute).Unix(), "exp": f.now.Add(time.Hour).Unix(),
		"email": "gabriel@example.org", "email_verified": true, "groups": []string{"family"},
	}
	for name, value := range f.override {
		if value == nil {
			delete(claims, name)
		} else {
			claims[name] = value
		}
	}
	writeJSON(w, map[string]any{"access_token": "access", "token_type": "Bearer", "id_token": f.sign(claims)})
}

// sign makes an RS256 JWT with the standard library only, so the tests do not
// depend on the code they test.
func (f *fakeIdP) sign(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("marshal claims: %v", err)
	}
	input := b64([]byte(`{"alg":"RS256","typ":"JWT","kid":"test-key"}`)) + "." + b64(payload)
	sum := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.signKey, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return input + "." + b64(signature)
}

// authorize plays the part of the provider after the user logged in: it checks
// the address Begin built and returns the code the browser comes back with.
func (f *fakeIdP) authorize(authURL string) string {
	f.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatalf("parse auth URL: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code", "client_id": testClientID, "redirect_uri": testRedirectURL,
		"scope": "openid email", "code_challenge_method": "S256",
	}
	for name, value := range want {
		if q.Get(name) != value {
			f.t.Errorf("auth URL: %s = %q, want %q", name, q.Get(name), value)
		}
	}
	if u.Path != "/authorize" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		f.t.Errorf("auth URL %q lacks the path, state, nonce or challenge", authURL)
	}
	code := fmt.Sprintf("code-%d", len(f.logins)+1000)
	f.mu.Lock()
	f.logins[code] = fakeLogin{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	f.mu.Unlock()
	return code
}

func (f *fakeIdP) set(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override = claims
}

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func newTestOIDC(f *fakeIdP, now *time.Time) *OIDC {
	return &OIDC{
		IssuerURL: f.server.URL, ClientID: testClientID, ClientSecret: testClientSecret,
		RedirectURL: testRedirectURL, Scopes: []string{"openid", "email"}, GroupsClaim: "groups",
		AllowedEmails: []string{"gabriel@example.org"}, AllowedGroups: []string{"ops"},
		SecureCookie: true, Client: f.server.Client(), Now: func() time.Time { return *now },
	}
}

// attempt is one login as the browser sees it: the cookie it holds and the
// query it comes back with.
type attempt struct {
	cookie *http.Cookie
	query  url.Values
}

func begin(t *testing.T, f *fakeIdP, o *OIDC) attempt {
	t.Helper()
	rec := httptest.NewRecorder()
	authURL, err := o.Begin(rec, httptest.NewRequest(http.MethodGet, "/login/oidc", nil))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	cookies := rec.Result().Cookies()
	if want := oidcCookiePrefix + u.Query().Get("state"); len(cookies) != 1 || cookies[0].Name != want {
		t.Fatalf("Begin set cookies %v, want just %s", cookies, want)
	}
	return attempt{cookie: cookies[0], query: url.Values{"code": {f.authorize(authURL)}, "state": {u.Query().Get("state")}}}
}

func finish(o *OIDC, a attempt) (string, *httptest.ResponseRecorder, error) {
	req := httptest.NewRequest(http.MethodGet, "/login/oidc/callback?"+a.query.Encode(), nil)
	if a.cookie != nil {
		req.AddCookie(a.cookie)
	}
	rec := httptest.NewRecorder()
	name, err := o.Finish(rec, req)
	return name, rec, err
}

func TestOIDCBeginSetsTheLoginCookie(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)

	a := begin(t, f, o) // also checks the address of the provider
	c := a.cookie
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/login/oidc" || c.MaxAge != 600 {
		t.Errorf("cookie = %+v, want HttpOnly, Secure, SameSite=Lax, path /login/oidc, 10 minutes", c)
	}
	if parts := strings.Split(c.Value, "."); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Errorf("cookie value %q is not nonce.verifier", c.Value)
	}

	// Without HTTPS (local development) the cookie must not be Secure.
	o.SecureCookie = false
	if begin(t, f, o).cookie.Secure {
		t.Error("the cookie is Secure although SecureCookie is off")
	}
	// Every login starts with new random values.
	if begin(t, f, o).cookie.Value == c.Value {
		t.Error("two logins share state, nonce and verifier")
	}
}

func TestOIDCFinish(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tests := []struct {
		name     string
		claims   map[string]any
		idp      func(*fakeIdP)
		attempt  func(*attempt)
		wantName string
		wantErr  string // empty: the login works
		// keepsCookie: the request has no state of a login under way, so it
		// must not touch the cookie of that login.
		keepsCookie bool
	}{
		{name: "allowed email", wantName: "gabriel@example.org"},
		{name: "email in other case", claims: map[string]any{"email": "Gabriel@Example.org"}, wantName: "gabriel@example.org"},
		{name: "email_verified as a string", claims: map[string]any{"email_verified": "true"}, wantName: "gabriel@example.org"},
		{name: "allowed group", claims: map[string]any{"email": "stranger@example.org", "groups": []string{"family", "ops"}}, wantName: "stranger@example.org"},
		{name: "group as one string", claims: map[string]any{"email": "stranger@example.org", "groups": "ops"}, wantName: "stranger@example.org"},
		{name: "allowed group without an email", claims: map[string]any{"email": nil, "groups": []string{"ops"}, "preferred_username": "gabi"}, wantName: "gabi"},
		{name: "name falls back to sub", claims: map[string]any{"email": nil, "groups": []string{"ops"}}, wantName: "user-1"},
		{name: "group of the right name but another claim", claims: map[string]any{"email": "stranger@example.org", "groups": nil, "roles": []string{"ops"}},
			wantErr: `no groups in the "groups" claim`},
		{name: "email not on the list", claims: map[string]any{"email": "stranger@example.org"}, wantErr: "no allowed email or group"},
		{name: "email not verified", claims: map[string]any{"email_verified": false}, wantErr: "does not say it is verified"},
		{name: "email_verified missing", claims: map[string]any{"email_verified": nil}, wantErr: "does not say it is verified"},
		{name: "unverified email does not become the name", claims: map[string]any{"email_verified": false, "groups": []string{"ops"}}, wantName: "user-1"},
		{name: "wrong nonce", claims: map[string]any{"nonce": "somebody-elses"}, wantErr: "nonce"},
		{name: "expired token", claims: map[string]any{"exp": testNow.Add(-time.Hour).Unix()}, wantErr: "expired"},
		{name: "wrong audience", claims: map[string]any{"aud": "another-client"}, wantErr: "expected audience"},
		{name: "wrong issuer", claims: map[string]any{"iss": "https://evil.example.org"}, wantErr: "different provider"},
		{name: "signed with another key", idp: func(f *fakeIdP) { f.signKey = otherKey }, wantErr: "failed to verify signature"},
		{name: "forged state", attempt: func(a *attempt) { a.query.Set("state", "forged") }, wantErr: "state does not match", keepsCookie: true},
		{name: "no state", attempt: func(a *attempt) { a.query.Del("state") }, wantErr: "state does not match", keepsCookie: true},
		{name: "no cookie", attempt: func(a *attempt) { a.cookie = nil }, wantErr: "no login is under way", keepsCookie: true},
		{name: "broken cookie", attempt: func(a *attempt) { a.cookie.Value = "abc" }, wantErr: "cookie is not valid"},
		{name: "no code", attempt: func(a *attempt) { a.query.Del("code") }, wantErr: "no code"},
		{name: "unknown code", attempt: func(a *attempt) { a.query.Set("code", "guessed") }, wantErr: "exchange the code"},
		{name: "refused by the provider", attempt: func(a *attempt) {
			a.query = url.Values{"error": {"access_denied"}, "state": a.query["state"]}
		}, wantErr: "access_denied"},
		// Somebody else sends the browser to the callback with an error: the
		// login under way goes on.
		{name: "error without the state", attempt: func(a *attempt) { a.query = url.Values{"error": {"access_denied"}} },
			wantErr: "access_denied", keepsCookie: true},
		{name: "wrong PKCE verifier", attempt: func(a *attempt) {
			nonce, _, _ := strings.Cut(a.cookie.Value, ".")
			a.cookie.Value = nonce + ".not-the-verifier-of-this-login"
		}, wantErr: "exchange the code"},
		{name: "groups claim missing", claims: map[string]any{"email": "stranger@example.org", "groups": nil},
			wantErr: `no groups in the "groups" claim`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := testNow
			f := newFakeIdP(t, now)
			o := newTestOIDC(f, &now)
			if tt.idp != nil {
				tt.idp(f)
			}
			a := begin(t, f, o)
			f.set(tt.claims)
			if tt.attempt != nil {
				tt.attempt(&a)
			}

			name, rec, err := finish(o, a)
			if tt.wantErr == "" {
				if err != nil || name != tt.wantName {
					t.Fatalf("Finish = %q, %v; want %q", name, err, tt.wantName)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Finish = %q, %v; want an error with %q", name, err, tt.wantErr)
			} else if name != "" {
				t.Errorf("a failed login returned the name %q", name)
			}

			cookies := rec.Result().Cookies()
			if tt.keepsCookie {
				if len(cookies) != 0 {
					t.Errorf("cookies after Finish = %v, want the login cookie left alone", cookies)
				}
				return
			}
			// Whatever the result, the cookie is deleted: one try per cookie.
			if len(cookies) != 1 || cookies[0].Name != a.cookie.Name || cookies[0].MaxAge >= 0 {
				t.Errorf("cookies after Finish = %v, want the login cookie deleted", cookies)
			}
		})
	}
}

func TestOIDCCodeWorksOnce(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	a := begin(t, f, o)
	if _, _, err := finish(o, a); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	if _, _, err := finish(o, a); err == nil {
		t.Error("the same code and cookie worked a second time")
	}
}

func TestOIDCProviderThatIsDownIsAskedAgainLater(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	f.down = true

	tryBegin := func() error {
		_, err := o.Begin(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/login/oidc", nil))
		return err
	}
	if err := tryBegin(); err == nil {
		t.Fatal("Begin worked although the provider is down")
	}
	f.mu.Lock()
	f.down = false
	asked := f.discoveries
	f.mu.Unlock()

	// The provider is back, but the failure is remembered for a while.
	now = testNow.Add(10 * time.Second)
	if err := tryBegin(); err == nil {
		t.Error("Begin worked within the wait after a failure")
	}
	f.mu.Lock()
	if f.discoveries != asked {
		t.Errorf("the provider was asked %d times during the wait, want none", f.discoveries-asked)
	}
	f.mu.Unlock()

	now = testNow.Add(31 * time.Second)
	if err := tryBegin(); err != nil {
		t.Fatalf("Begin after the wait: %v", err)
	}
	// Once it worked, the configuration is kept: no further discovery.
	f.mu.Lock()
	asked = f.discoveries
	f.mu.Unlock()
	if err := tryBegin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discoveries != asked {
		t.Error("the provider was asked again although discovery had worked")
	}
}

func TestOIDCErrorsHideTheSecrets(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	a := begin(t, f, o)
	code := a.query.Get("code")
	// A provider that repeats what it received in its error message.
	f.mu.Lock()
	f.tokenError = "cannot use secret " + testClientSecret + " with code " + code + "-extra"
	f.mu.Unlock()
	a.query.Set("code", code+"-extra")

	_, _, err := finish(o, a)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{testClientSecret, a.query.Get("code")} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error shows %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "[removed]") {
		t.Errorf("the error does not say that something was removed: %v", err)
	}
}

func TestOIDCHTTPClientTrustsTheGivenCA(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake below is expected
	server.StartTLS()
	t.Cleanup(server.Close)
	dir := t.TempDir()

	caFile := filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	withCA, err := NewOIDCHTTPClient(caFile)
	if err != nil {
		t.Fatalf("NewOIDCHTTPClient: %v", err)
	}
	if resp, err := withCA.Get(server.URL); err != nil {
		t.Errorf("a server with the given CA is refused: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	// Without the CA the certificate is not trusted.
	plain, err := NewOIDCHTTPClient("")
	if err != nil {
		t.Fatalf("NewOIDCHTTPClient: %v", err)
	}
	if resp, err := plain.Get(server.URL); err == nil {
		_ = resp.Body.Close()
		t.Error("a certificate from an unknown CA was accepted")
	}

	notPEM := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOIDCHTTPClient(notPEM); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("a file without a certificate: error = %v", err)
	}
	if _, err := NewOIDCHTTPClient(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing CA file must be an error")
	}
}

func TestOIDCHTTPClientDoesNotForwardAPost(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the redirect was followed")
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	client, err := NewOIDCHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(redirect.URL, "text/plain", strings.NewReader("client_secret=x"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the redirect itself", resp.StatusCode)
	}
}

func TestOIDCHTTPClientFollowsRedirectsOfAGet(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	t.Cleanup(redirect.Close)

	client, err := NewOIDCHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(redirect.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the target of the redirect", resp.StatusCode)
	}
}

func TestOIDCRedirectRules(t *testing.T) {
	request := func(method, address string) *http.Request {
		return httptest.NewRequest(method, address, nil)
	}
	tests := []struct {
		name   string
		via    []*http.Request
		next   *http.Request
		follow bool
	}{
		{"GET to https", []*http.Request{request("GET", "https://id.example.org/a")}, request("GET", "https://id.example.org/b"), true},
		{"POST", []*http.Request{request("POST", "https://id.example.org/token")}, request("GET", "https://id.example.org/b"), false},
		{"https to http", []*http.Request{request("GET", "https://id.example.org/a")}, request("GET", "http://id.example.org/b"), false},
		{"too many", []*http.Request{
			request("GET", "https://id.example.org/1"), request("GET", "https://id.example.org/2"),
			request("GET", "https://id.example.org/3"), request("GET", "https://id.example.org/4"),
			request("GET", "https://id.example.org/5"), request("GET", "https://id.example.org/6"),
		}, request("GET", "https://id.example.org/7"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkOIDCRedirect(tt.next, tt.via) == nil; got != tt.follow {
				t.Errorf("follow = %v, want %v", got, tt.follow)
			}
		})
	}
}

func TestOIDCTwoLoginsAtOnce(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	first, second := begin(t, f, o), begin(t, f, o)
	if first.cookie.Name == second.cookie.Name {
		t.Fatal("two logins share one cookie")
	}
	// The browser holds both cookies; each callback finds its own.
	for _, a := range []attempt{first, second} {
		req := httptest.NewRequest(http.MethodGet, "/login/oidc/callback?"+a.query.Encode(), nil)
		req.AddCookie(first.cookie)
		req.AddCookie(second.cookie)
		if _, err := o.Finish(httptest.NewRecorder(), req); err != nil {
			t.Errorf("Finish of the login with cookie %s: %v", a.cookie.Name, err)
		}
	}
}

func TestOIDCDiscoveryOutlivesTheRequest(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser went away before discovery began
	req := httptest.NewRequest(http.MethodGet, "/login/oidc", nil).WithContext(ctx)
	if _, err := o.Begin(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("Begin with a cancelled request: %v; the discovery must not depend on it", err)
	}
	begin(t, f, o) // and the next user is not blocked by a remembered failure
}

func TestOIDCTokenErrorKeepsItsType(t *testing.T) {
	now := testNow
	f := newFakeIdP(t, now)
	o := newTestOIDC(f, &now)
	a := begin(t, f, o)
	a.query.Set("code", "guessed-code")
	_, _, err := finish(o, a)
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) || retrieve.Response == nil || retrieve.Response.StatusCode != http.StatusBadRequest {
		t.Errorf("error %v does not unwrap to the RetrieveError of the 400 answer", err)
	}
}
