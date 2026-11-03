package notify

import (
	"context"
	"crypto/tls"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

var mailTestTime = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func newTestMail(s *fakeServer, mode string) *Mail {
	return &Mail{
		Host:      "127.0.0.1",
		Port:      s.port(),
		TLS:       mode,
		From:      "smokealarm@example.com",
		To:        []string{"me@example.com"},
		TLSConfig: &tls.Config{RootCAs: s.roots, MinVersion: tls.VersionTLS12},
		Now:       func() time.Time { return mailTestTime },
	}
}

func oneProblem() Notification {
	return Notification{New: []Alert{crashLoop()}, BaseURL: "https://smokealarm.example.com"}
}

// parse splits a received message into its headers and decoded body.
func parse(t *testing.T, data string) (mail.Header, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("received data is not a valid message: %v\n%s", err, data)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return msg.Header, string(body)
}

func TestMailDeliversMessage(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := server.received()
	if len(got) != 1 {
		t.Fatalf("server received %d messages, want 1", len(got))
	}
	if got[0].from != "smokealarm@example.com" || strings.Join(got[0].to, ",") != "me@example.com" {
		t.Errorf("envelope = %s -> %v", got[0].from, got[0].to)
	}

	header, body := parse(t, got[0].data)
	if want := "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin"; header.Get("Subject") != want {
		t.Errorf("Subject = %q, want %q", header.Get("Subject"), want)
	}
	if header.Get("From") != "<smokealarm@example.com>" || header.Get("To") != "<me@example.com>" {
		t.Errorf("From = %q, To = %q", header.Get("From"), header.Get("To"))
	}
	if date, err := header.Date(); err != nil || !date.Equal(mailTestTime) {
		t.Errorf("Date = %v, %v; want %v", date, err, mailTestTime)
	}
	if !strings.HasSuffix(header.Get("Message-Id"), "@example.com>") {
		t.Errorf("Message-ID = %q", header.Get("Message-Id"))
	}
	if header.Get("Content-Type") != "text/plain; charset=UTF-8" {
		t.Errorf("Content-Type = %q", header.Get("Content-Type"))
	}
	wantBody, err := Body(oneProblem())
	if err != nil {
		t.Fatal(err)
	}
	// SMTP uses CRLF line ends; compare with normalized ones.
	if strings.ReplaceAll(body, "\r\n", "\n") != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestMailSeveralRecipients(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)
	m.To = []string{"me@example.com", "\"Ops Team\" <ops@example.com>"}

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := server.received()
	if len(got) != 1 || strings.Join(got[0].to, ",") != "me@example.com,ops@example.com" {
		t.Fatalf("recipients = %+v, want both addresses in one message", got)
	}
	header, _ := parse(t, got[0].data)
	if to := header.Get("To"); !strings.Contains(to, "me@example.com") || !strings.Contains(to, "ops@example.com") {
		t.Errorf("To header = %q, want both recipients", to)
	}
}

func TestMailAuthentication(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone) // localhost may use PLAIN without TLS
	m.Username, m.Password = "alice", "s3cret"

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := server.received()
	if len(got) != 1 || got[0].authUser != "alice" || got[0].authPass != "s3cret" {
		t.Errorf("server saw %+v, want the login alice/s3cret", got)
	}
}

func TestMailWithoutUsernameDoesNotAuthenticate(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{rejectAuth: true})
	m := newTestMail(server, TLSNone)

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v (no login must be attempted without a username)", err)
	}
}

func TestMailRejectedLoginDoesNotLeakPassword(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{rejectAuth: true})
	m := newTestMail(server, TLSNone)
	m.Username, m.Password = "alice", "s3cret-password"

	err := m.Send(context.Background(), oneProblem())
	if err == nil {
		t.Fatal("expected an error for rejected credentials")
	}
	if strings.Contains(err.Error(), "s3cret-password") {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "login") || len(server.received()) != 0 {
		t.Errorf("error = %v, received %d; want a login error and no message", err, len(server.received()))
	}
}

func TestMailStartTLS(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{offerStartTLS: true})
	m := newTestMail(server, TLSStartTLS)
	m.Username, m.Password = "alice", "s3cret" // non-localhost rules are met: the link is encrypted

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := server.received()
	if len(got) != 1 || !got[0].usedTLS || got[0].authUser != "alice" {
		t.Errorf("got %+v, want one message delivered over TLS with login", got)
	}
}

func TestMailImplicitTLS(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{implicitTLS: true})
	m := newTestMail(server, TLSImplicit)

	if err := m.Send(context.Background(), oneProblem()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := server.received(); len(got) != 1 || !got[0].usedTLS {
		t.Errorf("got %+v, want one message delivered over TLS", got)
	}
}

func TestMailUntrustedCertificateIsRejected(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{implicitTLS: true})
	m := newTestMail(server, TLSImplicit)
	m.TLSConfig = nil // system roots do not know the self-signed certificate

	if err := m.Send(context.Background(), oneProblem()); err == nil {
		t.Error("expected a certificate error")
	}
	if len(server.received()) != 0 {
		t.Error("nothing must be delivered over an untrusted connection")
	}
}

func TestMailStartTLSRequiredButNotOffered(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{offerStartTLS: false})
	m := newTestMail(server, TLSStartTLS)

	err := m.Send(context.Background(), oneProblem())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("got %v, want an error about missing STARTTLS (no silent downgrade)", err)
	}
	if len(server.received()) != 0 {
		t.Error("nothing must be delivered without encryption")
	}
}

func TestMailRejectedRecipient(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{rejectRcpt: "ops@example.com"})
	m := newTestMail(server, TLSNone)
	m.To = []string{"me@example.com", "ops@example.com"}

	err := m.Send(context.Background(), oneProblem())
	if err == nil || !strings.Contains(err.Error(), "rcpt ops@example.com") {
		t.Fatalf("got %v, want a recipient error", err)
	}
	if len(server.received()) != 0 {
		t.Error("a message with a rejected recipient must not be sent")
	}
}

func TestMailServerUnreachable(t *testing.T) {
	m := &Mail{
		Host: "127.0.0.1", Port: closedPort(t), TLS: TLSNone,
		From: "smokealarm@example.com", To: []string{"me@example.com"},
		Now: func() time.Time { return mailTestTime },
	}
	err := m.Send(context.Background(), oneProblem())
	if err == nil || !strings.Contains(err.Error(), "connect to 127.0.0.1:") {
		t.Errorf("got %v, want a connection error naming the server", err)
	}
}

func TestMailSubjectCannotInjectHeaders(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)

	evil := crashLoop()
	evil.Reason = "Oops\r\nBcc: evil@example.com"
	if err := m.Send(context.Background(), Notification{New: []Alert{evil}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := server.received()
	if len(got) != 1 {
		t.Fatalf("received %d messages", len(got))
	}
	header, _ := parse(t, got[0].data)
	if bcc := header.Get("Bcc"); bcc != "" {
		t.Errorf("header injection worked: Bcc = %q", bcc)
	}
	if !strings.Contains(header.Get("Subject"), "Oops") {
		t.Errorf("Subject = %q", header.Get("Subject"))
	}
}

func TestMailNonASCIIText(t *testing.T) {
	server := newFakeServer(t, fakeServerOptions{})
	m := newTestMail(server, TLSNone)

	alert := crashLoop()
	alert.Reason = "Störung"
	alert.Message = "Container läuft nicht – bitte prüfen"
	if err := m.Send(context.Background(), Notification{New: []Alert{alert}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := server.received()
	if len(got) != 1 {
		t.Fatalf("received %d messages", len(got))
	}
	header, body := parse(t, got[0].data)
	// The subject is an encoded word; the standard decoder must recover it.
	decoded, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
	if err != nil || !strings.Contains(decoded, "Störung") {
		t.Errorf("decoded subject = %q, %v", decoded, err)
	}
	if !strings.Contains(body, "Container läuft nicht – bitte prüfen") {
		t.Errorf("body lost its non-ASCII text: %q", body)
	}
}

func TestMailInvalidAddresses(t *testing.T) {
	m := &Mail{Host: "127.0.0.1", Port: 1, TLS: TLSNone, From: "nonsense", To: []string{"me@example.com"}, Now: time.Now}
	if err := m.Send(context.Background(), oneProblem()); err == nil || !strings.Contains(err.Error(), "sender") {
		t.Errorf("got %v, want an error about the sender", err)
	}

	m = &Mail{Host: "127.0.0.1", Port: 1, TLS: TLSNone, From: "a@example.com", Now: time.Now}
	if err := m.Send(context.Background(), oneProblem()); err == nil || !strings.Contains(err.Error(), "recipients") {
		t.Errorf("got %v, want an error about missing recipients", err)
	}
}

// Mail and Log must both be usable as a Notifier.
func TestNotifiersImplementInterface(t *testing.T) {
	notifiers := []Notifier{&Mail{}, &Log{}}
	if notifiers[0].Name() != "mail" || notifiers[1].Name() != "log" {
		t.Error("unexpected notifier names")
	}
}
