package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Timeouts so that an unreachable or stuck mail server cannot block the main
// loop for long. They are real network deadlines, not part of any logic, so
// they use the real clock.
const (
	dialTimeout = 10 * time.Second
	sendTimeout = 30 * time.Second
)

// TLS modes for Mail.TLS.
const (
	TLSStartTLS = "starttls" // plain connection, upgraded with STARTTLS (port 587)
	TLSImplicit = "tls"      // TLS from the first byte (port 465)
	TLSNone     = "none"     // no encryption, for local testing only
)

// Mail sends notifications over SMTP, as a text and an HTML part
// (multipart/alternative), or as text only (see Templates.mailHTML).
type Mail struct {
	Host     string
	Port     int
	TLS      string // one of the TLS* constants
	Username string // empty means no authentication
	Password string
	From     string
	To       []string

	// TLSConfig customizes certificate checking. Nil means system roots and
	// the server name taken from Host; tests use it for self-signed certificates.
	TLSConfig *tls.Config
	// Now is injectable so the Date header is predictable in tests.
	Now func() time.Time
	// Templates are the user's message templates; nil means the built-in ones.
	Templates *Templates
}

// Name identifies the target.
func (m *Mail) Name() string { return "mail" }

// Description names the mail server, which is not a secret.
func (m *Mail) Description() string { return "mail (" + m.Host + ")" }

// Send delivers one mail with all changes. Errors never contain the password.
// A *FallbackError means the mail went out with a built-in template.
func (m *Mail) Send(ctx context.Context, n Notification) error {
	subject, subjectFallback := m.Templates.subject("mail", n)
	body, bodyFallback, err := m.Templates.mailBody(n)
	if err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	html, htmlFallback, err := m.Templates.mailHTML(n)
	if err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	from, recipients, err := m.addresses()
	if err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	msg, err := m.buildMessage(from, recipients, subject, body, html)
	if err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	if err := m.deliver(ctx, from.Address, recipients, msg); err != nil {
		return err
	}
	return fallbacks(subjectFallback, bodyFallback, htmlFallback)
}

// addresses parses the configured sender and recipients.
func (m *Mail) addresses() (*mail.Address, []*mail.Address, error) {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return nil, nil, fmt.Errorf("parse sender %q: %w", m.From, err)
	}
	var recipients []*mail.Address
	for _, to := range m.To {
		addr, err := mail.ParseAddress(to)
		if err != nil {
			return nil, nil, fmt.Errorf("parse recipient %q: %w", to, err)
		}
		recipients = append(recipients, addr)
	}
	if len(recipients) == 0 {
		return nil, nil, errors.New("no recipients configured")
	}
	return from, recipients, nil
}

// buildMessage assembles headers and body: the text alone, or the text and
// the HTML as two alternatives, text first, because mail programs show the
// last one they can. Every part is quoted-printable because Kubernetes
// messages can contain non-ASCII text, which strict mail servers do not
// accept as 8bit.
func (m *Mail) buildMessage(from *mail.Address, to []*mail.Address, subject, text, html string) ([]byte, error) {
	messageID, err := newMessageID(from.Address)
	if err != nil {
		return nil, err
	}
	var toHeader []string
	for _, addr := range to {
		toHeader = append(toHeader, addr.String())
	}

	var msg bytes.Buffer
	headers := []struct{ name, value string }{
		{"From", from.String()},
		{"To", strings.Join(toHeader, ", ")},
		{"Subject", encodeSubject(subject)},
		{"Date", m.Now().Format(time.RFC1123Z)},
		{"Message-ID", messageID},
		{"MIME-Version", "1.0"},
	}
	for _, h := range headers {
		msg.WriteString(h.name + ": " + h.value + "\r\n")
	}

	if html == "" {
		msg.WriteString("Content-Type: " + textPlain + "\r\n")
		msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQuotedPrintable(&msg, text); err != nil {
			return nil, err
		}
		return msg.Bytes(), nil
	}

	parts := multipart.NewWriter(&msg)
	msg.WriteString("Content-Type: multipart/alternative; boundary=" + parts.Boundary() + "\r\n\r\n")
	for _, part := range []struct{ contentType, content string }{
		{textPlain, text},
		{"text/html; charset=UTF-8", html},
	} {
		w, err := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		if err := writeQuotedPrintable(w, part.content); err != nil {
			return nil, err
		}
	}
	if err := parts.Close(); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	return msg.Bytes(), nil
}

// textPlain is the content type of the text part.
const textPlain = "text/plain; charset=UTF-8"

// writeQuotedPrintable writes a text quoted-printable encoded.
func writeQuotedPrintable(w io.Writer, text string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(text)); err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	return nil
}

// encodeSubject removes line breaks (they would let a value start new header
// lines) and encodes non-ASCII characters as RFC 2047 words.
func encodeSubject(subject string) string {
	oneLine := strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	return mime.QEncoding.Encode("utf-8", oneLine)
}

// newMessageID creates a unique id in the domain of the sender.
func newMessageID(sender string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create message id: %w", err)
	}
	domain := "localhost"
	if _, after, found := strings.Cut(sender, "@"); found {
		domain = after
	}
	return "<" + hex.EncodeToString(random) + "@" + domain + ">", nil
}

// deliver talks SMTP. net/smtp.SendMail is not used because it cannot do
// implicit TLS (port 465).
func (m *Mail) deliver(ctx context.Context, from string, to []*mail.Address, msg []byte) error {
	conn, err := m.connect(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(sendTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: set deadline: %w", err)
	}

	client, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: greeting from %s: %w", m.address(), err)
	}
	defer func() { _ = client.Close() }()

	if err := m.secure(client); err != nil {
		return err
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mail: sender %s rejected: %w", from, err)
	}
	for _, addr := range to {
		if err := client.Rcpt(addr.Address); err != nil {
			return fmt.Errorf("mail: rcpt %s rejected: %w", addr.Address, err)
		}
	}
	if err := writeData(client, msg); err != nil {
		return err
	}

	// The server has accepted the message. A failing QUIT must not turn this
	// into an error, because the engine would then send the mail again.
	_ = client.Quit()
	return nil
}

// connect opens the TCP connection, with TLS from the start if configured.
func (m *Mail) connect(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if m.TLS == TLSImplicit {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: m.tlsConfig()}
		conn, err = tlsDialer.DialContext(ctx, "tcp", m.address())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", m.address())
	}
	if err != nil {
		return nil, fmt.Errorf("mail: connect to %s: %w", m.address(), err)
	}
	return conn, nil
}

// secure upgrades the connection with STARTTLS if configured and logs in if
// a username is set.
func (m *Mail) secure(client *smtp.Client) error {
	if m.TLS == TLSStartTLS {
		// Refusing to continue without STARTTLS prevents a silent downgrade
		// to an unencrypted connection.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("mail: %s does not offer STARTTLS; use SMOKEALARM_SMTP_TLS=tls or none if that is intended", m.address())
		}
		if err := client.StartTLS(m.tlsConfig()); err != nil {
			return fmt.Errorf("mail: starttls with %s: %w", m.address(), err)
		}
	}
	if m.Username != "" {
		// PlainAuth itself refuses to send credentials over an unencrypted
		// connection, except to localhost.
		auth := smtp.PlainAuth("", m.Username, m.Password, m.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mail: login as %s failed: %w", m.Username, err)
		}
	}
	return nil
}

func writeData(client *smtp.Client, msg []byte) error {
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: start data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mail: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: message not accepted: %w", err)
	}
	return nil
}

func (m *Mail) address() string {
	return net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
}

func (m *Mail) tlsConfig() *tls.Config {
	if m.TLSConfig == nil {
		return &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}
	}
	cfg := m.TLSConfig.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = m.Host
	}
	return cfg
}
