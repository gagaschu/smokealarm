package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServerOptions selects how the fake SMTP server behaves.
type fakeServerOptions struct {
	implicitTLS   bool   // TLS from the first byte
	offerStartTLS bool   // announce the STARTTLS extension
	rejectRcpt    string // recipient address that gets a 550
	rejectAuth    bool   // answer every login with 535
}

// receivedMail is what the fake server saw for one delivered message.
type receivedMail struct {
	from     string
	to       []string
	data     string
	authUser string
	authPass string
	usedTLS  bool
}

// fakeServer is a minimal SMTP server, just enough to test Mail.
type fakeServer struct {
	options  fakeServerOptions
	listener net.Listener
	cert     tls.Certificate
	roots    *x509.CertPool

	mu       sync.Mutex
	messages []receivedMail
}

func newFakeServer(t *testing.T, options fakeServerOptions) *fakeServer {
	t.Helper()
	cert, roots := selfSignedCert(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{options: options, listener: listener, cert: cert, roots: roots}
	t.Cleanup(func() { _ = listener.Close() })

	go s.acceptLoop()
	return s
}

func (s *fakeServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *fakeServer) received() []receivedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]receivedMail{}, s.messages...)
}

func (s *fakeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.serve(conn)
	}
}

// serve speaks just enough SMTP for one session.
func (s *fakeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12}

	usedTLS := false
	if s.options.implicitTLS {
		conn = tls.Server(conn, tlsConfig)
		usedTLS = true
	}
	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	var current receivedMail
	reply("220 fake ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250-fake")
			if s.options.offerStartTLS && !usedTLS {
				reply("250-STARTTLS")
			}
			reply("250-AUTH PLAIN")
			reply("250 OK")
		case upper == "STARTTLS":
			reply("220 go ahead")
			conn = tls.Server(conn, tlsConfig)
			reader = bufio.NewReader(conn)
			reply = func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
			usedTLS = true
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			if s.options.rejectAuth {
				reply("535 bad credentials")
				continue
			}
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := strings.Split(string(decoded), "\x00") // identity, user, password
			if len(parts) == 3 {
				current.authUser, current.authPass = parts[1], parts[2]
			}
			reply("235 ok")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			current.from = addressIn(line)
			reply("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := addressIn(line)
			if addr == s.options.rejectRcpt {
				reply("550 no such user")
				continue
			}
			current.to = append(current.to, addr)
			reply("250 ok")
		case upper == "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			current.data = readData(reader)
			current.usedTLS = usedTLS
			s.mu.Lock()
			s.messages = append(s.messages, current)
			s.mu.Unlock()
			current = receivedMail{authUser: current.authUser, authPass: current.authPass}
			reply("250 queued")
		case upper == "QUIT":
			reply("221 bye")
			return
		default:
			reply("500 unknown command")
		}
	}
}

// addressIn extracts the address from "MAIL FROM:<a@b>" or "RCPT TO:<a@b>".
func addressIn(line string) string {
	start, end := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if start < 0 || end < start {
		return ""
	}
	return line[start+1 : end]
}

// readData reads the message up to the lone "." line and removes dot stuffing.
func readData(reader *bufio.Reader) string {
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return data.String()
		}
		if strings.TrimRight(line, "\r\n") == "." {
			return data.String()
		}
		data.WriteString(strings.TrimPrefix(line, "."))
	}
}

// selfSignedCert creates a certificate valid for 127.0.0.1 and a pool that
// trusts it, so tests can check real TLS handshakes.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// closedPort returns a port on which nothing is listening.
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
