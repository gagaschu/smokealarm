package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/auth"
)

const (
	oidcTestClientID = "smokealarm"
	oidcTestRedirect = "https://smokealarm.example.org/login/oidc/callback"
)

// webIdP is a small identity provider for the handler tests. It answers
// discovery, the keys and the token request, and signs a real ID token for
// the nonce of the login that was started. The details of the flow (PKCE,
// every kind of bad token) are tested in the auth package.
type webIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu    sync.Mutex
	nonce string // of the login that was started
	email string // who logs in at the provider
}

func newWebIdP(t *testing.T) *webIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &webIdP{key: key, email: "gabriel@example.org"}
	b64 := base64.RawURLEncoding.EncodeToString
	reply := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{
			"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize",
			"token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		claims := map[string]any{
			"iss": p.server.URL, "aud": oidcTestClientID, "sub": "user-1", "nonce": p.nonce,
			"iat": testNow.Add(-time.Minute).Unix(), "exp": testNow.Add(time.Hour).Unix(),
			"email": p.email, "email_verified": true,
		}
		p.mu.Unlock()
		payload, _ := json.Marshal(claims)
		input := b64([]byte(`{"alg":"RS256","typ":"JWT","kid":"k"}`)) + "." + b64(payload)
		sum := sha256.Sum256([]byte(input))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Errorf("sign: %v", err)
		}
		reply(w, map[string]any{"access_token": "a", "token_type": "Bearer", "id_token": input + "." + b64(signature)})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// newOIDCEnv is a test server with the OIDC login switched on.
func newOIDCEnv(t *testing.T, name string) (*testEnv, *webIdP) {
	t.Helper()
	idp := newWebIdP(t)
	e := newTestEnv(t)
	e.server.OIDC = &auth.OIDC{
		IssuerURL: idp.server.URL, ClientID: oidcTestClientID, ClientSecret: "client-secret-value-123",
		RedirectURL: oidcTestRedirect, Scopes: []string{"openid", "email"}, GroupsClaim: "groups",
		AllowedEmails: []string{"gabriel@example.org"}, SecureCookie: true,
		Client: idp.server.Client(), Now: func() time.Time { return testNow },
	}
	e.server.OIDCName = name
	e.server.Sessions.OIDCCredentials = auth.OIDCFingerprint(idp.server.URL, oidcTestClientID, "groups", []string{"gabriel@example.org"}, nil)
	var err error
	if e.handler, err = e.server.Handler(); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return e, idp
}

// cookieNamed returns the cookie with this name from a response.
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// oidcLoginCookie returns the OIDC login cookie from a response: its name starts
// with smokealarm_oidc_ and ends with the state.
func oidcLoginCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, "smokealarm_oidc_") {
			return c
		}
	}
	return nil
}

// startOIDC plays the browser up to the provider: it follows the redirect from
// /login/oidc, lets the provider remember the nonce, and returns the callback
// path (with code and state) and the login cookie.
func startOIDC(t *testing.T, e *testEnv, idp *webIdP) (callback string, cookie *http.Cookie) {
	t.Helper()
	rec := e.do(http.MethodGet, "/login/oidc", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /login/oidc = %d, want 303; body: %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), idp.server.URL+"/authorize?") {
		t.Fatalf("redirect to %q, want the authorize endpoint of the provider", rec.Header().Get("Location"))
	}
	cookie = oidcLoginCookie(rec)
	if cookie == nil || cookie.Name != "smokealarm_oidc_"+location.Query().Get("state") {
		t.Fatalf("/login/oidc set the login cookie %+v, want one named after the state", cookie)
	}
	idp.mu.Lock()
	idp.nonce = location.Query().Get("nonce")
	idp.mu.Unlock()
	return "/login/oidc/callback?" + url.Values{"code": {"the-code"}, "state": {location.Query().Get("state")}}.Encode(), cookie
}

func TestNoOIDCNoButtonAndNoRoutes(t *testing.T) {
	e := newTestEnv(t)
	page := e.do(http.MethodGet, "/login", nil, nil)
	if strings.Contains(page.Body.String(), "/login/oidc") || strings.Contains(page.Body.String(), "Login with") {
		t.Error("the login page offers OIDC although it is off")
	}
	for _, path := range []string{"/login/oidc", "/login/oidc/callback?code=x&state=y"} {
		if rec := e.do(http.MethodGet, path, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 while OIDC is off", path, rec.Code)
		}
	}
}

func TestLoginPageShowsTheButtonNextToThePasswordForm(t *testing.T) {
	tests := []struct {
		name, oidcName string
		want, notWant  string
	}{
		{"own name", "MeineID", "Login with MeineID", "Login with OIDC"},
		{"special characters are escaped", "<b>x</b>", "Login with &lt;b&gt;x&lt;/b&gt;", "<b>x</b>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newOIDCEnv(t, tt.oidcName)
			body := e.do(http.MethodGet, "/login", nil, nil).Body.String()
			if !strings.Contains(body, tt.want) || !strings.Contains(body, `href="/login/oidc"`) {
				t.Errorf("login page lacks %q or the link; body:\n%s", tt.want, body)
			}
			if tt.notWant != "" && strings.Contains(body, tt.notWant) {
				t.Errorf("login page contains %q", tt.notWant)
			}
			if !strings.Contains(body, `name="password"`) {
				t.Error("the password form is gone; it must stay next to OIDC")
			}
		})
	}
}

func TestOIDCLoginEndToEnd(t *testing.T) {
	e, idp := newOIDCEnv(t, "MeineID")
	callback, loginCookie := startOIDC(t, e, idp)

	req := httptest.NewRequest(http.MethodGet, callback, nil)
	req.AddCookie(loginCookie)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 303 to /; body: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	session := cookieNamed(rec, "smokealarm_session")
	if session == nil || !session.HttpOnly || !session.Secure {
		t.Fatalf("session cookie = %+v, want one that is HttpOnly and Secure", session)
	}
	if deleted := oidcLoginCookie(rec); deleted == nil || deleted.MaxAge >= 0 {
		t.Errorf("the login cookie was not deleted: %+v", deleted)
	}

	// The session works for the protected pages.
	if page := e.do(http.MethodGet, "/", nil, session); page.Code != http.StatusOK {
		t.Errorf("GET / with the OIDC session = %d, want 200", page.Code)
	}
	// And it ends when the allowlist changes.
	e.server.Sessions.OIDCCredentials = auth.OIDCFingerprint(idp.server.URL, oidcTestClientID, "groups", []string{"somebody-else@example.org"}, nil)
	if page := e.do(http.MethodGet, "/", nil, session); page.Code != http.StatusSeeOther {
		t.Errorf("GET / after the allowlist changed = %d, want a redirect to the login", page.Code)
	}
}

func TestOIDCLoginOfSomeoneNotOnTheAllowlist(t *testing.T) {
	e, idp := newOIDCEnv(t, "OIDC")
	idp.email = "stranger@example.org"
	callback, loginCookie := startOIDC(t, e, idp)

	req := httptest.NewRequest(http.MethodGet, callback, nil)
	req.AddCookie(loginCookie)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("callback = %d, want 401", rec.Code)
	}
	if cookieNamed(rec, "smokealarm_session") != nil {
		t.Error("a session was started for someone who is not on the allowlist")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Login with OIDC failed.") {
		t.Errorf("the page does not say that the login failed:\n%s", body)
	}
	// The reason is for the log: it must not tell the visitor about the list.
	if strings.Contains(body, "allowed") || strings.Contains(body, "stranger@example.org") {
		t.Errorf("the page gives the reason away:\n%s", body)
	}
}

func TestOIDCCallbackWithoutTheLoginCookie(t *testing.T) {
	e, idp := newOIDCEnv(t, "OIDC")
	callback, _ := startOIDC(t, e, idp)

	rec := e.do(http.MethodGet, callback, nil, nil) // another browser, no cookie
	if rec.Code != http.StatusUnauthorized || cookieNamed(rec, "smokealarm_session") != nil {
		t.Errorf("callback without a cookie = %d with session %v, want 401 and no session",
			rec.Code, cookieNamed(rec, "smokealarm_session"))
	}
	if strings.Contains(rec.Body.String(), "no login is under way") {
		t.Error("the page shows the internal reason")
	}
}

func TestOIDCStartWhileTheProviderIsDown(t *testing.T) {
	e, idp := newOIDCEnv(t, "MeineID")
	idp.server.Close()

	rec := e.do(http.MethodGet, "/login/oidc", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /login/oidc = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Login with MeineID is not available right now.") {
		t.Errorf("the page does not say that OIDC is not available:\n%s", body)
	}
	if !strings.Contains(body, `name="password"`) {
		t.Error("the password form must stay available when the provider is down")
	}
	if oidcLoginCookie(rec) != nil {
		t.Error("a login cookie was set although the login could not start")
	}
}

func TestPasswordLoginStillWorksWithOIDCOn(t *testing.T) {
	e, _ := newOIDCEnv(t, "OIDC")
	if cookie := e.login(); e.do(http.MethodGet, "/", nil, cookie).Code != http.StatusOK {
		t.Error("the password session does not work while OIDC is on")
	}
}
