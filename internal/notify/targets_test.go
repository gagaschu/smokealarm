package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP targets are tested against a local server: no network is needed.

// received is what the test server got.
type received struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// newTarget starts a server that records the request and answers with the
// given status.
func newTarget(t *testing.T, status int) (*httptest.Server, *received) {
	t.Helper()
	got := &received{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = received{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, got
}

// newEcho starts a server that refuses every request and repeats the whole
// request in its answer, the worst case for leaking a secret into an error.
func newEcho(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad request for "+r.URL.String()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Api-Key"))
	}))
	t.Cleanup(server.Close)
	return server
}

// closedURL is the address of a server that is no longer there.
func closedURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	return server.URL
}

// checkGoldenJSON compares a JSON body with a golden file, indented for
// reading.
func checkGoldenJSON(t *testing.T, name string, body []byte) {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	checkGolden(t, name, out.String()+"\n")
}

func wantRequest(t *testing.T, got *received, path, contentType string) {
	t.Helper()
	if got.method != http.MethodPost || got.path != path {
		t.Errorf("request = %s %s, want POST %s", got.method, got.path, path)
	}
	if ct := got.header.Get("Content-Type"); ct != contentType {
		t.Errorf("Content-Type = %q, want %q", ct, contentType)
	}
}

// wantNoSecret fails if the error is missing or shows a secret.
func wantNoSecret(t *testing.T, err error, wantText string, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), wantText) {
		t.Errorf("error %q does not contain %q", err, wantText)
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q contains the secret %q", err, secret)
		}
	}
}

func TestWebhook(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	w := &Webhook{URL: server.URL + "/hooks/smokealarm", HeaderName: "X-Api-Key", HeaderValue: "s3cret-key", Client: NewHTTPClient()}

	if err := w.Send(context.Background(), mixed()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantRequest(t, got, "/hooks/smokealarm", "application/json")
	if key := got.header.Get("X-Api-Key"); key != "s3cret-key" {
		t.Errorf("X-Api-Key = %q", key)
	}
	checkGoldenJSON(t, "webhook_mixed.golden", got.body)
	if w.Description() != "webhook" {
		t.Errorf("Description = %q", w.Description())
	}
}

func TestWebhookListsAreNeverNull(t *testing.T) {
	server, got := newTarget(t, http.StatusNoContent)
	w := &Webhook{URL: server.URL, Client: NewHTTPClient()}
	if err := w.Send(context.Background(), Notification{Resolved: []Alert{resolved(crashLoop())}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Contains(got.body, []byte(`"new":[]`)) || !bytes.Contains(got.body, []byte(`"reminders":[]`)) {
		t.Errorf("body = %s, want empty lists", got.body)
	}
	if got.header.Get("Authorization") != "" {
		t.Error("no header is configured, so none must be sent")
	}
}

func TestWebhookKeepsItsSecrets(t *testing.T) {
	echo := newEcho(t)
	w := &Webhook{URL: echo.URL + "/hook/path-token-123", HeaderName: "Authorization", HeaderValue: "Bearer header-token-456", Client: NewHTTPClient()}
	err := w.Send(context.Background(), mixed())
	wantNoSecret(t, err, "400", "path-token-123", "header-token-456")

	w.URL = closedURL(t) + "/hook/path-token-123"
	err = w.Send(context.Background(), mixed())
	wantNoSecret(t, err, "webhook", "path-token-123", "header-token-456")
}

func TestNtfy(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	n := &Ntfy{Server: server.URL + "/", Topic: "smokealarm-x7k2", Token: "tk_access", Client: NewHTTPClient()}

	if err := n.Send(context.Background(), mixed()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantRequest(t, got, "/smokealarm-x7k2", "text/plain; charset=utf-8")
	for header, want := range map[string]string{
		"Title":         "[smokealarm] 2 new problems, 1 still critical, 1 resolved",
		"Priority":      "urgent",
		"Tags":          "rotating_light",
		"Click":         testBaseURL,
		"Authorization": "Bearer tk_access",
	} {
		if value := got.header.Get(header); value != want {
			t.Errorf("%s = %q, want %q", header, value, want)
		}
	}
	checkGolden(t, "ntfy_mixed.golden", string(got.body))
	if n.Description() != "ntfy" {
		t.Errorf("Description = %q, want the name only", n.Description())
	}
}

func TestNtfyWithoutTokenAndBaseURL(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	n := &Ntfy{Server: server.URL, Topic: "alerts", Client: NewHTTPClient()}
	if err := n.Send(context.Background(), Notification{Resolved: []Alert{resolved(crashLoop())}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.header.Get("Authorization") != "" || got.header.Get("Click") != "" || got.header.Get("Priority") != "low" {
		t.Errorf("headers = %v", got.header)
	}
}

func TestNtfyKeepsItsSecrets(t *testing.T) {
	echo := newEcho(t)
	n := &Ntfy{Server: echo.URL, Topic: "secret-topic-789", Token: "tk_secret", Client: NewHTTPClient()}
	wantNoSecret(t, n.Send(context.Background(), mixed()), "400", "secret-topic-789", "tk_secret")

	n.Server = closedURL(t)
	wantNoSecret(t, n.Send(context.Background(), mixed()), "ntfy", "secret-topic-789", "tk_secret")
}

const testBotToken = "123456:AAH-bot-token_xyz"

func TestTelegram(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	tg := &Telegram{APIURL: server.URL, Token: testBotToken, ChatID: "-1001234567890", Client: NewHTTPClient()}

	if err := tg.Send(context.Background(), mixed()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantRequest(t, got, "/bot"+testBotToken+"/sendMessage", "application/json")
	var message struct {
		ChatID    string         `json:"chat_id"`
		Text      string         `json:"text"`
		ParseMode string         `json:"parse_mode"`
		Preview   map[string]any `json:"link_preview_options"`
	}
	if err := json.Unmarshal(got.body, &message); err != nil {
		t.Fatalf("body: %v", err)
	}
	if message.ChatID != "-1001234567890" || message.ParseMode != "" || message.Preview["is_disabled"] != true {
		t.Errorf("message = %+v, want plain text to the chat without a link preview", message)
	}
	checkGolden(t, "telegram_mixed.golden", message.Text)
	if tg.Name() != "telegram:-1001234567890" || tg.Description() != "telegram (chat -1001234567890)" {
		t.Errorf("Name, Description = %q, %q", tg.Name(), tg.Description())
	}
}

func TestTelegramMessageFitsTheLimit(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	tg := &Telegram{APIURL: server.URL, Token: testBotToken, ChatID: "@smokealarm", Client: NewHTTPClient()}
	if err := tg.Send(context.Background(), manyPods(500, testBaseURL)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var message struct{ Text string }
	if err := json.Unmarshal(got.body, &message); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(message.Text) > telegramLimit || !strings.HasPrefix(message.Text, "[smokealarm] 500 new problems, 1 resolved\n\n") ||
		!strings.Contains(message.Text, "more, see "+testBaseURL) {
		t.Errorf("text (%d bytes) is too long or not cut properly:\n%s", len(message.Text), message.Text)
	}
}

func TestTelegramKeepsTheToken(t *testing.T) {
	echo := newEcho(t)
	tg := &Telegram{APIURL: echo.URL, Token: testBotToken, ChatID: "42", Client: NewHTTPClient()}
	wantNoSecret(t, tg.Send(context.Background(), mixed()), "400", testBotToken, "AAH-bot-token_xyz")

	tg.APIURL = closedURL(t)
	wantNoSecret(t, tg.Send(context.Background(), mixed()), "telegram", testBotToken, "AAH-bot-token_xyz")
}

func TestSlack(t *testing.T) {
	server, got := newTarget(t, http.StatusOK)
	s := &Slack{WebhookURL: server.URL + "/services/T000/B000/XXXX", Client: NewHTTPClient()}
	if err := s.Send(context.Background(), mixed()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantRequest(t, got, "/services/T000/B000/XXXX", "application/json")
	checkGoldenJSON(t, "slack_mixed.golden", got.body)
}

func TestSlackKeepsTheWebhookURL(t *testing.T) {
	echo := newEcho(t)
	s := &Slack{WebhookURL: echo.URL + "/services/T000/B000/slack-secret", Client: NewHTTPClient()}
	wantNoSecret(t, s.Send(context.Background(), mixed()), "400", "slack-secret")

	s.WebhookURL = closedURL(t) + "/services/T000/B000/slack-secret"
	wantNoSecret(t, s.Send(context.Background(), mixed()), "slack", "slack-secret")
}

func TestTeams(t *testing.T) {
	server, got := newTarget(t, http.StatusAccepted) // what Workflows answers
	tm := &Teams{WebhookURL: server.URL + "/workflows/abc/triggers/manual/paths/invoke", Client: NewHTTPClient()}
	if err := tm.Send(context.Background(), mixed()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantRequest(t, got, "/workflows/abc/triggers/manual/paths/invoke", "application/json")
	checkGoldenJSON(t, "teams_mixed.golden", got.body)
}

func TestTeamsWithoutBaseURLHasNoButton(t *testing.T) {
	server, got := newTarget(t, http.StatusAccepted)
	tm := &Teams{WebhookURL: server.URL, Client: NewHTTPClient()}
	if err := tm.Send(context.Background(), Notification{New: []Alert{crashLoop()}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if bytes.Contains(got.body, []byte("Action.OpenUrl")) {
		t.Errorf("body = %s, want no button without a base URL", got.body)
	}
}

func TestTeamsKeepsTheWebhookURL(t *testing.T) {
	echo := newEcho(t)
	tm := &Teams{WebhookURL: echo.URL + "/workflows/sig=teams-secret", Client: NewHTTPClient()}
	wantNoSecret(t, tm.Send(context.Background(), mixed()), "400", "teams-secret")

	tm.WebhookURL = closedURL(t) + "/workflows/sig=teams-secret"
	wantNoSecret(t, tm.Send(context.Background(), mixed()), "teams", "teams-secret")
}

// A redirect is an error: following it would send the message somewhere else.
func TestRedirectIsNotFollowed(t *testing.T) {
	_, got := newTarget(t, http.StatusOK)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/elsewhere", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	s := &Slack{WebhookURL: redirect.URL + "/services/secret", Client: NewHTTPClient()}
	wantNoSecret(t, s.Send(context.Background(), mixed()), "302", "/services/secret")
	if got.method != "" {
		t.Error("the redirect was followed")
	}
}

// The response body in an error is short.
func TestErrorBodyIsCut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 5000))
	}))
	t.Cleanup(server.Close)
	err := (&Teams{WebhookURL: server.URL, Client: NewHTTPClient()}).Send(context.Background(), mixed())
	if err == nil || !strings.Contains(err.Error(), "500") || len(err.Error()) > 300 {
		t.Errorf("error = %v, want the status and a short excerpt", err)
	}
}

// The run's context ends a request that hangs.
func TestRequestUsesTheContext(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Slack{WebhookURL: server.URL, Client: NewHTTPClient()}).Send(ctx, mixed())
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %v, want the cancellation", err)
	}
}

// newAnswer starts a server that refuses every request with this answer.
func newAnswer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server
}

// A server may quote only a part of the URL or of the header.
func TestPartsOfASecretAreRemoved(t *testing.T) {
	const pathToken = "0f8fad5b-d9cb-469f"
	const queryToken = "sig/with+escapes"
	const headerToken = "header-token-456"
	secretURL := "/webhook/" + pathToken + "/a%2Fb-escaped-part?sig=sig%2Fwith%2Bescapes"
	for _, answer := range []string{
		`The webhook "POST ` + pathToken + `" is not registered`,
		"unknown path a%2Fb-escaped-part",
		"bad signature " + queryToken,
		"bad token " + headerToken,
	} {
		server := newAnswer(t, answer)
		w := &Webhook{URL: server.URL + secretURL, HeaderName: "Authorization", HeaderValue: "Bearer " + headerToken, Client: NewHTTPClient()}
		err := w.Send(context.Background(), mixed())
		wantNoSecret(t, err, "404", pathToken, "a%2Fb-escaped-part", queryToken, headerToken)
	}
}

// Secrets are removed before the excerpt is cut, so the cut cannot leave the
// start of one behind.
func TestCutDoesNotLeaveAPartOfASecret(t *testing.T) {
	const token = "very-secret-token-0123456789"
	for offset := maxErrorBody - len(token); offset <= maxErrorBody; offset++ {
		server := newAnswer(t, strings.Repeat("x", offset)+token)
		err := (&Telegram{APIURL: server.URL, Token: token, ChatID: "1", Client: NewHTTPClient()}).Send(context.Background(), mixed())
		wantNoSecret(t, err, "404", token[:len(token)/2])
	}
}

// A target is shown by its name only, also when it cannot be reached.
func TestDialErrorDoesNotShowTheHost(t *testing.T) {
	w := &Webhook{URL: "https://relay.invalid/hook", Client: NewHTTPClient()}
	wantNoSecret(t, w.Send(context.Background(), mixed()), "webhook", "relay.invalid")
}

// Teams counts the whole card, and every line costs a block of its own.
func TestTeamsMessageFitsTheLimit(t *testing.T) {
	for _, count := range []int{10, 150, 400, 5000} {
		var alerts []Alert
		for i := range count {
			a := mixed().New[0]
			a.Key = fmt.Sprintf("key-%d", i)
			alerts = append(alerts, a)
		}
		server, got := newTarget(t, http.StatusAccepted)
		n := Notification{New: alerts, BaseURL: "https://smokealarm.example.org"}
		if err := (&Teams{WebhookURL: server.URL, Client: NewHTTPClient()}).Send(context.Background(), n); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(got.body) > teamsMaxMessage {
			t.Errorf("%d alerts: message of %d bytes, want at most %d", count, len(got.body), teamsMaxMessage)
		}
		if count >= 400 && !bytes.Contains(got.body, []byte("more, see https://smokealarm.example.org")) {
			t.Errorf("%d alerts: the message does not say that it was cut", count)
		}
	}
}
