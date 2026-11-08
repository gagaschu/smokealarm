package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// templateDir writes the files into a new directory and returns its path.
func templateDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadTemplates loads the files and fails the test on an error.
func loadTemplates(t *testing.T, files map[string]string) *Templates {
	t.Helper()
	templates, err := LoadTemplates(templateDir(t, files), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	return templates
}

func TestLoadTemplatesWithoutDirectory(t *testing.T) {
	templates, err := LoadTemplates("", slog.New(slog.DiscardHandler))
	if err != nil || templates != nil {
		t.Fatalf("LoadTemplates(\"\") = %v, %v; want nil, nil", templates, err)
	}
	if got := templates.Describe("slack"); got != "built-in" {
		t.Errorf("Describe = %q, want built-in", got)
	}
	subject, fallback := templates.subject("slack", mixed())
	if subject != Subject(mixed()) || fallback != nil {
		t.Errorf("subject = %q, %v; want the built-in one", subject, fallback)
	}
}

func TestLoadTemplatesErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // parts of the error
	}{
		{"unknown name", map[string]string{"slak.chat.tmpl": "x"}, []string{"slak.chat.tmpl", "unknown"}},
		{"no chat template for mail", map[string]string{"mail.chat.tmpl": "x"}, []string{"mail.chat.tmpl", "unknown"}},
		{"too large", map[string]string{"chat.tmpl": strings.Repeat("x", maxTemplateSize+1)}, []string{"chat.tmpl", "larger"}},
		{"syntax error", map[string]string{"slack.chat.tmpl": "ok\n{{if .New}}"}, []string{"slack.chat.tmpl:2"}},
		{"unknown field", map[string]string{"subject.tmpl": "{{.Sinse}}"}, []string{"subject.tmpl", "Sinse"}},
		{"unknown field of a problem", map[string]string{"mail.tmpl": "{{range .New}}{{.Sinse}}{{end}}"}, []string{"mail.tmpl", "Sinse"}},
		{"only when shortened", map[string]string{"chat.tmpl": "x{{if .Omitted}}{{.Nope}}{{end}}"}, []string{"chat.tmpl", "Nope"}},
		{"only for one target", map[string]string{"chat.tmpl": `x{{if eq .Target "teams"}}{{.Nope}}{{end}}`}, []string{"chat.tmpl", "Nope"}},
		{"only for affected problems", map[string]string{"chat.tmpl": "x{{range .NodesDown}}{{range .Affected}}{{.Nope}}{{end}}{{end}}"}, []string{"Nope"}},
		{"only for a reminder alone", map[string]string{"subject.tmpl": "{{if .Resolved}}R{{else}}{{(index .New 0).Reason}}{{end}}"}, []string{"subject.tmpl", "index"}},
		{"only for a node down alone", map[string]string{"subject.tmpl": "{{if or .Resolved .Reminders}}R{{else}}{{(index .New 0).Reason}}{{end}}"}, []string{"subject.tmpl", "index"}},
		{"empty", map[string]string{"ntfy.subject.tmpl": "{{/* nothing */}}  \n"}, []string{"ntfy.subject.tmpl", "empty"}},
		{"several", map[string]string{"chat.tmpl": "{{.A}}", "mail.tmpl": "{{.B}}"}, []string{"chat.tmpl", "mail.tmpl"}},
		{"html only as mail.html.tmpl", map[string]string{"html.tmpl": "x"}, []string{"html.tmpl", "unknown"}},
		{"no html for chat targets", map[string]string{"slack.html.tmpl": "x"}, []string{"slack.html.tmpl", "unknown"}},
		{"html syntax error", map[string]string{"mail.html.tmpl": "<p>\n{{if .New}}</p>"}, []string{"mail.html.tmpl:2"}},
		{"html unknown field", map[string]string{"mail.html.tmpl": "<p>{{range .Resolved}}{{.Sinse}}{{end}}</p>"}, []string{"mail.html.tmpl", "Sinse"}},
		{"html empty", map[string]string{"mail.html.tmpl": "  \n"}, []string{"mail.html.tmpl", "empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadTemplates(templateDir(t, tt.files), slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("want an error")
			}
			for _, part := range tt.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestLoadTemplatesMissingDirectory(t *testing.T) {
	_, err := LoadTemplates(filepath.Join(t.TempDir(), "nothing-here"), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "nothing-here") {
		t.Errorf("error = %v, want one that names the directory", err)
	}
}

// A ConfigMap volume holds its files in a hidden directory and links to them;
// everything that is not a template is left alone.
func TestLoadTemplatesFromAConfigMapVolume(t *testing.T) {
	dir := templateDir(t, map[string]string{
		"..2026_11_06_10_00_00.123/chat.tmpl": "from the ConfigMap",
		".hidden.tmpl":                        "{{.Broken",
		"README.md":                           "notes",
		"old.tmpl/whatever":                   "a directory with a .tmpl name",
	})
	if err := os.Symlink("..2026_11_06_10_00_00.123", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "chat.tmpl"), filepath.Join(dir, "chat.tmpl")); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	templates, err := LoadTemplates(dir, slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	text, fallback := templates.chatBody("slack", mixed(), 3000)
	if text != "from the ConfigMap" || fallback != nil {
		t.Errorf("chatBody = %q, %v", text, fallback)
	}
	// Only the file that may be a typo is logged, not the hidden entries or
	// the directory.
	if got := log.String(); strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "file=README.md") {
		t.Errorf("log = %q, want one warning about README.md", got)
	}
}

// A template with the wrong ending keeps the built-in message, so it is
// logged: "chat.tpl" is a typo more often than not.
func TestLoadTemplatesWarnsAboutAWrongEnding(t *testing.T) {
	var log bytes.Buffer
	templates, err := LoadTemplates(templateDir(t, map[string]string{"chat.tpl": "x"}), slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil || templates != nil {
		t.Fatalf("LoadTemplates = %v, %v; want nil, nil", templates, err)
	}
	if !strings.Contains(log.String(), "file=chat.tpl") {
		t.Errorf("log = %q, want a warning about chat.tpl", log.String())
	}
}

func TestTemplatePrecedence(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"subject.tmpl":       "general {{.Target}}",
		"slack.subject.tmpl": "only slack",
		"chat.tmpl":          "general chat for {{.Target}}",
		"teams.chat.tmpl":    "only teams",
	})
	n := mixed()
	for _, tt := range []struct{ kind, wantSubject, wantChat string }{
		{"slack", "only slack", "general chat for slack"},
		{"teams", "general teams", "only teams"},
		{"ntfy", "general ntfy", "general chat for ntfy"},
		{"mail", "general mail", ""},
	} {
		if got, _ := templates.subject(tt.kind, n); got != tt.wantSubject {
			t.Errorf("subject for %s = %q, want %q", tt.kind, got, tt.wantSubject)
		}
		if tt.wantChat == "" {
			continue
		}
		if got, _ := templates.chatBody(tt.kind, n, 3000); got != tt.wantChat {
			t.Errorf("chat for %s = %q, want %q", tt.kind, got, tt.wantChat)
		}
	}
	body, fallback, err := templates.mailBody(newTemplateData(n, "mail"))
	want, _ := Body(n)
	if err != nil || fallback != nil || body != want {
		t.Errorf("mail body without mail.tmpl = %q, %v, %v; want the built-in one", body, fallback, err)
	}
}

// The HTML part: the built-in one, the user's, or none when the user has a
// text template only, which the HTML would hide.
func TestMailHTMLPrecedence(t *testing.T) {
	n := mixed()
	builtin, err := HTMLBody(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no templates", nil, builtin},
		{"a subject only", map[string]string{"subject.tmpl": "S"}, builtin},
		{"a text template only", map[string]string{"mail.tmpl": "M"}, ""},
		{"an HTML template only", map[string]string{"mail.html.tmpl": "<p>{{.Target}}</p>"}, "<p>mail</p>"},
		{"both", map[string]string{"mail.tmpl": "M", "mail.html.tmpl": "<p>H</p>"}, "<p>H</p>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var templates *Templates
			if tt.files != nil {
				templates = loadTemplates(t, tt.files)
			}
			html, fallback := templates.mailHTML(newTemplateData(n, "mail"))
			if fallback != nil || html != tt.want {
				t.Errorf("mailHTML = %q, %v; want %q", html, fallback, tt.want)
			}
		})
	}
}

// A user HTML template that fails with a real notification sends the
// built-in HTML and says so; the text part is not affected.
func TestMailHTMLFallsBackAtRunTime(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"mail.html.tmpl": "{{if gt (len .New) 5}}{{index .New 9}}{{end}}<p>fine</p>",
	})
	n := manyPods(6, testBaseURL)
	html, fallback := templates.mailHTML(newTemplateData(n, "mail"))
	builtin, _ := HTMLBody(n)
	if html != builtin {
		t.Errorf("html = %q, want the built-in one", html)
	}
	if fallback == nil || fallback.File != "mail.html.tmpl" || fallback.TextOnly {
		t.Fatalf("fallback = %v, want one for mail.html.tmpl with the built-in HTML", fallback)
	}

	send, sent := templateTarget(t, "mail", templates, 0)
	var target *FallbackError
	if err := send(n); !errors.As(err, &target) || target.File != "mail.html.tmpl" {
		t.Errorf("Send = %v, want the fallback for mail.html.tmpl", err)
	}
	if want, _ := Body(n); !strings.HasSuffix(sent(), want) {
		t.Errorf("the text part changed:\n%s", sent())
	}
}

// A user HTML template that fails next to a user text template must not
// bring back the built-in HTML, which would hide the user's text.
func TestFailingHTMLTemplateWithTextTemplateSendsTextOnly(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"mail.tmpl":      "Eigener Text: {{.Summary}}",
		"mail.html.tmpl": "{{if gt (len .New) 5}}{{index .New 9}}{{end}}<p>fine</p>",
	})
	n := manyPods(6, testBaseURL)
	html, fallback := templates.mailHTML(newTemplateData(n, "mail"))
	if html != "" {
		t.Errorf("html = %q, want none", html)
	}
	if fallback == nil || fallback.File != "mail.html.tmpl" || !fallback.TextOnly || !strings.Contains(fallback.Error(), "text only") {
		t.Fatalf("fallback = %v, want a text-only one for mail.html.tmpl", fallback)
	}

	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)
	m.Templates = templates
	var target *FallbackError
	if err := m.Send(context.Background(), n); !errors.As(err, &target) || !target.TextOnly {
		t.Errorf("Send = %v, want the text-only fallback", err)
	}
	header, text, _ := parseParts(t, server.received()[0].data)
	if header.Get("Content-Type") != "text/plain; charset=UTF-8" || !strings.HasPrefix(text, "Eigener Text") {
		t.Errorf("Content-Type = %q, text = %q; want the user's text alone", header.Get("Content-Type"), text)
	}
}

// A mail with a user text template and no HTML template goes out as text only.
func TestMailWithTextTemplateOnlyIsTextOnly(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)
	m.Templates = loadTemplates(t, map[string]string{"mail.tmpl": "Eigener Text: {{.Summary}}"})
	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	header, text, html := parseParts(t, server.received()[0].data)
	if header.Get("Content-Type") != "text/plain; charset=UTF-8" || html != "" {
		t.Errorf("Content-Type = %q, html = %q; want text only", header.Get("Content-Type"), html)
	}
	if strings.TrimSpace(text) != "Eigener Text: 1 new problem" {
		t.Errorf("text = %q", text)
	}
}

func TestDescribeAndUnusedFiles(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"subject.tmpl":       "S",
		"mail.tmpl":          "M",
		"mail.html.tmpl":     "<p>H</p>",
		"slack.chat.tmpl":    "C",
		"teams.subject.tmpl": "T",
	})
	for kind, want := range map[string]string{
		"mail":     "subject.tmpl, mail.tmpl, mail.html.tmpl",
		"slack":    "subject.tmpl, slack.chat.tmpl",
		"teams":    "teams.subject.tmpl",
		"telegram": "subject.tmpl",
		"webhook":  "",
	} {
		if got := templates.Describe(kind); got != want {
			t.Errorf("Describe(%s) = %q, want %q", kind, got, want)
		}
	}
	if got := strings.Join(templates.UnusedFiles([]string{"mail", "slack"}), ","); got != "teams.subject.tmpl" {
		t.Errorf("UnusedFiles = %q", got)
	}
	if got := strings.Join(templates.UnusedFiles([]string{"webhook"}), ","); got != "mail.html.tmpl,mail.tmpl,slack.chat.tmpl,subject.tmpl,teams.subject.tmpl" {
		t.Errorf("UnusedFiles for the webhook only = %q", got)
	}
}

// A template that fails only with data the startup check did not have falls
// back to the built-in template and says so.
func TestTemplateFallsBackAtRunTime(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"chat.tmpl":    "{{if gt (len .New) 5}}{{index .New 9}}{{end}}fine",
		"subject.tmpl": "{{range .New}}{{.Name}} {{end}}",
	})
	n := manyPods(6, testBaseURL)
	text, fallback := templates.chatBody("ntfy", n, 4000)
	if text != ChatBody(n, 4000) {
		t.Errorf("chat = %q, want the built-in text", text)
	}
	if fallback == nil || fallback.File != "chat.tmpl" || !strings.Contains(fallback.Error(), "index") {
		t.Fatalf("fallback = %v, want one for chat.tmpl", fallback)
	}
	var asError error = fallback
	var target *FallbackError
	if !errors.As(fmt.Errorf("ntfy: %w", asError), &target) {
		t.Error("errors.As does not find the FallbackError")
	}

	// Nothing to say is a failure too: resolved problems only.
	resolvedOnly := Notification{Resolved: []Alert{resolved(crashLoop())}}
	subject, fallback := templates.subject("ntfy", resolvedOnly)
	if subject != Subject(resolvedOnly) || fallback == nil || !strings.Contains(fallback.Error(), "empty") {
		t.Errorf("subject = %q, %v; want the built-in one and an error", subject, fallback)
	}
}

func TestUserChatTemplateIsShortened(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"chat.tmpl": "{{range .New}}{{.Name}}\n{{end}}{{if .Omitted}}+{{.Omitted}}{{end}}",
	})
	n := manyPods(40, testBaseURL) // 40 new problems and one resolved
	text, fallback := templates.chatBody("slack", n, 100)
	if fallback != nil || len(text) > 100 {
		t.Fatalf("chat = %q (%d bytes), %v", text, len(text), fallback)
	}
	shown := strings.Count(text, "app-")
	if !strings.HasSuffix(text, fmt.Sprintf("+%d", 41-shown)) || shown == 0 {
		t.Errorf("chat = %q: %d shown, the rest not counted", text, shown)
	}
}

func TestUserChatTemplateWithTooMuchFixedText(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"chat.tmpl": strings.Repeat("Größe ", 200) + "{{len .New}}",
	})
	text, fallback := templates.chatBody("slack", mixed(), 100)
	if fallback != nil || len(text) > 100 || !utf8.ValidString(text) || !strings.HasSuffix(text, "…") {
		t.Errorf("chat = %q (%d bytes), %v; want it cut at a character", text, len(text), fallback)
	}
}

func TestUserSubjectIsOneLine(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"subject.tmpl": "Cluster pi:\n{{.Summary}}\r\nBcc: evil@example.com",
	})
	got, _ := templates.subject("mail", mixed())
	if strings.ContainsAny(got, "\r\n") || !strings.HasPrefix(got, "Cluster pi: NODE DOWN") {
		t.Errorf("subject = %q, want one line", got)
	}
}

// The German templates in testdata/templates/de show every field; the golden
// files are what a reader gets.
func TestGermanTemplatesGolden(t *testing.T) {
	templates, err := LoadTemplates(filepath.Join("testdata", "templates", "de"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	n := mixed()
	subject, fallback := templates.subject("mail", n)
	body, bodyFallback, err := templates.mailBody(newTemplateData(n, "mail"))
	if err != nil || fallback != nil || bodyFallback != nil {
		t.Fatalf("mail: %v, %v, %v", err, fallback, bodyFallback)
	}
	checkGolden(t, "de_mail.golden", subject+"\n\n"+body)
	html, htmlFallback := templates.mailHTML(newTemplateData(n, "mail"))
	if htmlFallback != nil {
		t.Fatalf("mail HTML: %v", htmlFallback)
	}
	checkGolden(t, "de_mail.html.golden", html)

	chat, fallback := templates.chatBody("ntfy", n, 4000)
	if fallback != nil {
		t.Fatal(fallback)
	}
	checkGolden(t, "de_chat.golden", chat)

	short, fallback := templates.chatBody("ntfy", manyPods(40, testBaseURL), 600)
	if fallback != nil || len(short) > 600 {
		t.Fatalf("short chat: %d bytes, %v", len(short), fallback)
	}
	checkGolden(t, "de_chat_cut.golden", short)
}

// Every target uses the templates, and reports a fallback only after the
// message went out.
func TestTargetsUseTheTemplates(t *testing.T) {
	good := loadTemplates(t, map[string]string{
		"subject.tmpl": "Betreff\nfür {{.Target}}",
		"mail.tmpl":    "Mail-Text {{len .New}}",
		"chat.tmpl":    "Chat-Text für {{.Target}}",
	})
	broken := loadTemplates(t, map[string]string{
		"chat.tmpl": "{{if gt (len .New) 5}}{{index .New 9}}{{end}}Chat",
		"mail.tmpl": "{{if gt (len .New) 5}}{{index .New 9}}{{end}}Mail",
	})
	for _, kind := range []string{"mail", "ntfy", "telegram", "slack", "teams"} {
		t.Run(kind, func(t *testing.T) {
			send, sent := templateTarget(t, kind, good, http.StatusOK)
			if err := send(mixed()); err != nil {
				t.Fatalf("Send: %v", err)
			}
			got := sent()
			// A line break shows as one in headers and as \n in JSON.
			if !strings.Contains(got, "Betreff für "+kind) || strings.Contains(got, "Betreff\n") || strings.Contains(got, `Betreff\n`) {
				t.Errorf("the subject is missing or not on one line:\n%s", got)
			}
			if want := map[bool]string{true: "Mail-Text 2", false: "Chat-Text für " + kind}[kind == "mail"]; !strings.Contains(got, want) {
				t.Errorf("%q is missing:\n%s", want, got)
			}

			send, sent = templateTarget(t, kind, broken, http.StatusOK)
			err := send(manyPods(6, testBaseURL))
			var fallback *FallbackError
			if !errors.As(err, &fallback) {
				t.Fatalf("Send = %v, want a FallbackError", err)
			}
			if got := sent(); !strings.Contains(got, "app-005") {
				t.Errorf("the built-in message did not go out:\n%s", got)
			}

			if kind == "mail" {
				return // the fake SMTP server does not refuse
			}
			send, _ = templateTarget(t, kind, broken, http.StatusInternalServerError)
			if err := send(manyPods(6, testBaseURL)); err == nil || errors.As(err, &fallback) {
				t.Errorf("Send = %v, want the send error, not the fallback", err)
			}
		})
	}
}

// templateTarget builds a target of the kind with the templates, against a
// local server that answers with status. sent returns all that the server got,
// headers included, as text.
func templateTarget(t *testing.T, kind string, templates *Templates, status int) (send func(Notification) error, sent func() string) {
	t.Helper()
	ctx := context.Background()
	if kind == "mail" {
		server := newFakeServer(t, fakeServerOptions{})
		m := newTestMail(server, TLSNone)
		m.Templates = templates
		return func(n Notification) error { return m.Send(ctx, n) }, func() string {
			messages := server.received()
			if len(messages) == 0 {
				return ""
			}
			header, body := parse(t, messages[len(messages)-1].data)
			subject, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
			if err != nil {
				t.Fatalf("decode subject: %v", err)
			}
			return "Subject: " + subject + "\n" + body
		}
	}
	server, got := newTarget(t, status)
	var target Notifier
	switch kind {
	case "ntfy":
		target = &Ntfy{Server: server.URL, Topic: "topic", Client: NewHTTPClient(), Templates: templates}
	case "telegram":
		target = &Telegram{APIURL: server.URL, Token: "token", ChatID: "1", Client: NewHTTPClient(), Templates: templates}
	case "slack":
		target = &Slack{WebhookURL: server.URL, Client: NewHTTPClient(), Templates: templates}
	case "teams":
		target = &Teams{WebhookURL: server.URL, Client: NewHTTPClient(), Templates: templates}
	}
	return func(n Notification) error { return target.Send(ctx, n) }, func() string {
		return "Title: " + got.header.Get("Title") + "\n" + string(got.body)
	}
}

// A subject may list every problem, but it is cut, so that it fits into a
// mail header line and leaves room for the Telegram text. A long one once
// made the Telegram target panic.
func TestLongSubjectIsCut(t *testing.T) {
	templates := loadTemplates(t, map[string]string{"subject.tmpl": "{{range .New}}{{.Object}} {{end}}x"})
	n := manyPods(500, testBaseURL)
	subject, fallback := templates.subject("telegram", n)
	if fallback != nil || len(subject) > maxSubjectLength || !strings.HasSuffix(subject, "…") {
		t.Errorf("subject = %q (%d bytes), %v; want it cut at %d bytes", subject, len(subject), fallback, maxSubjectLength)
	}
	for _, kind := range []string{"ntfy", "telegram", "slack", "teams"} {
		send, sent := templateTarget(t, kind, templates, http.StatusOK)
		if err := send(n); err != nil {
			t.Errorf("%s: Send = %v", kind, err)
		}
		if kind == "telegram" {
			var message struct{ Text string }
			if err := json.Unmarshal([]byte(strings.TrimPrefix(sent(), "Title: \n")), &message); err != nil {
				t.Fatal(err)
			}
			if len(message.Text) > telegramLimit {
				t.Errorf("telegram text is %d bytes, more than %d", len(message.Text), telegramLimit)
			}
		}
	}

	// Non-ASCII characters take three times the room once encoded; the
	// header line still stays below the 998 bytes a mail line may have.
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)
	m.Templates = loadTemplates(t, map[string]string{"subject.tmpl": "{{range .New}}Störung {{.Name}} {{end}}"})
	if err := m.Send(context.Background(), n); err != nil {
		t.Fatalf("mail: Send = %v", err)
	}
	for _, line := range strings.Split(string(server.received()[0].data), "\r\n") {
		if len(line) > 998 {
			t.Errorf("mail line of %d bytes: %.80s…", len(line), line)
		}
	}
}

// A template that lists only problems says nothing without them. If not even
// one problem fits, that is no reason for the built-in template: the text with
// one problem is cut.
func TestChatTextIsCutIfNotEvenOneProblemFits(t *testing.T) {
	templates := loadTemplates(t, map[string]string{"chat.tmpl": "{{range .New}}{{.Object}}: {{.Message}}\n{{end}}"})
	text, fallback := templates.chatBody("slack", manyPods(3, ""), 40)
	if fallback != nil || len(text) > 40 || !strings.HasPrefix(text, "Pod media/app-000: ") || !strings.HasSuffix(text, "…") {
		t.Errorf("chat = %q, %v; want the first problem, cut", text, fallback)
	}
}

// When the subject and the text both fall back, both are named.
func TestBothFallbacksAreReported(t *testing.T) {
	templates := loadTemplates(t, map[string]string{
		"subject.tmpl": "{{if gt (len .New) 5}}{{index .New 9}}{{end}}S",
		"chat.tmpl":    "{{if gt (len .New) 5}}{{index .New 9}}{{end}}C",
	})
	send, _ := templateTarget(t, "ntfy", templates, http.StatusOK)
	err := send(manyPods(6, testBaseURL))
	var fallback *FallbackError
	if !errors.As(err, &fallback) || !strings.Contains(err.Error(), "subject.tmpl") || !strings.Contains(err.Error(), "chat.tmpl") {
		t.Errorf("Send = %v, want fallbacks for subject.tmpl and chat.tmpl", err)
	}
}
