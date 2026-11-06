package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// testToken stands in for the ServiceAccount token.
const testToken = "service-account-token"

// fakeKubelet is an HTTPS server that answers like a kubelet. Its certificate
// is valid for 127.0.0.1, like a kubelet serving certificate with the node IP.
func fakeKubelet(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	// Refused handshakes are what some tests are about; keep them out of the output.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// statsHandler answers /stats/summary with summaryJSON, but only with the
// right token, as a kubelet with webhook authentication would.
func statsHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/summary" {
			t.Errorf("path = %q, want /stats/summary", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, summaryJSON)
	}
}

// nodeAt is a Ready node whose kubelet listens at the address of rawURL.
func nodeAt(t *testing.T, name, rawURL string) *corev1.Node {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	node := readyNode(name, corev1.ConditionTrue)
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: name},
		{Type: corev1.NodeInternalIP, Address: host},
	}
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(port)
	return node
}

// caFileOf writes the server's certificate to a file, as a CA for the client.
func caFileOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// kubeletClientFor builds the real kubelet client, trusting server.
func kubeletClientFor(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client, err := NewKubeletClient(&rest.Config{BearerToken: testToken}, caFileOf(t, server))
	if err != nil {
		t.Fatalf("NewKubeletClient: %v", err)
	}
	return client
}

// httpsCluster is a cluster that asks the kubelets through client, without
// replacing the fetcher.
func httpsCluster(t *testing.T, client *http.Client) *Cluster {
	t.Helper()
	cluster, err := New(fake.NewClientset(), nil, Options{KubeletClient: client},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cluster
}

func TestKubeletStatsAreReadDirectly(t *testing.T) {
	server := fakeKubelet(t, statsHandler(t))
	cluster := httpsCluster(t, kubeletClientFor(t, server))

	usage, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if reason != "" || len(usage) != 2 || usage["data/db-data"].UsedBytes != 9700 {
		t.Errorf("usage %v, reason %q; want the 2 claims of the summary", usage, reason)
	}
}

func TestKubeletRefusesTheRequest(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			server := fakeKubelet(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Forbidden (user=system:serviceaccount:smokealarm:smokealarm, verb=get, resource=nodes, subresource=stats)", code)
			})
			cluster := httpsCluster(t, kubeletClientFor(t, server))

			usage, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
			if usage != nil {
				t.Errorf("usage = %v, want none", usage)
			}
			// The kubelet's own answer says which user and subresource it checked.
			for _, want := range []string{"pi-1", "HTTP " + strconv.Itoa(code), "subresource=stats", "nodes/stats", "SMOKEALARM_DISABLED_CHECKS=pvc-usage"} {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not contain %q", reason, want)
				}
			}
		})
	}
}

func TestKubeletUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close() // nothing listens there any more

	server := fakeKubelet(t, statsHandler(t)) // only for a trusted CA
	cluster := httpsCluster(t, kubeletClientFor(t, server))

	_, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", "https://"+address)})
	for _, want := range []string{"could not reach the kubelet of node pi-1", address, "NetworkPolicy"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
}

func TestKubeletCertificateNotTrusted(t *testing.T) {
	server := fakeKubelet(t, statsHandler(t))
	// Without a CA file and without CAData only the system CAs are trusted,
	// which did not sign the test certificate.
	client, err := NewKubeletClient(&rest.Config{BearerToken: testToken}, "")
	if err != nil {
		t.Fatalf("NewKubeletClient: %v", err)
	}
	cluster := httpsCluster(t, client)

	usage, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if usage != nil {
		t.Errorf("usage = %v, want none", usage)
	}
	for _, want := range []string{"could not verify the TLS certificate of the kubelet on node pi-1", "SMOKEALARM_KUBELET_CA_FILE"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
}

// A kubeconfig that skips verification for the API server must not make the
// kubelet client skip it too.
func TestKubeletClientAlwaysVerifies(t *testing.T) {
	server := fakeKubelet(t, statsHandler(t))
	config := &rest.Config{BearerToken: testToken, TLSClientConfig: rest.TLSClientConfig{Insecure: true, ServerName: "kubernetes"}}
	client, err := NewKubeletClient(config, "")
	if err != nil {
		t.Fatalf("NewKubeletClient: %v", err)
	}
	if !config.Insecure || config.ServerName != "kubernetes" {
		t.Error("NewKubeletClient changed the config of the API client")
	}

	_, reason := httpsCluster(t, client).collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if !strings.Contains(reason, "could not verify the TLS certificate") {
		t.Errorf("reason %q; want a certificate error although the config said insecure", reason)
	}
}

// The ServerName of the API client must not be checked against a node: with
// it cleared, the CA of the API client (CAData) is enough.
func TestKubeletClientUsesTheAPIClientCA(t *testing.T) {
	server := fakeKubelet(t, statsHandler(t))
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	config := &rest.Config{BearerToken: testToken, TLSClientConfig: rest.TLSClientConfig{CAData: ca, ServerName: "kubernetes.default.svc"}}
	client, err := NewKubeletClient(config, "")
	if err != nil {
		t.Fatalf("NewKubeletClient: %v", err)
	}

	usage, reason := httpsCluster(t, client).collectVolumeUsage(context.Background(), []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if reason != "" || len(usage) != 2 {
		t.Errorf("usage %v, reason %q; want the stats", usage, reason)
	}
}

func TestKubeletClientBadCAFile(t *testing.T) {
	notPEM := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(t.TempDir(), "nope.crt"), "not PEM": notPEM} {
		t.Run(name, func(t *testing.T) {
			_, err := NewKubeletClient(&rest.Config{}, path)
			if err == nil || !strings.Contains(err.Error(), "SMOKEALARM_KUBELET_CA_FILE") {
				t.Errorf("err = %v, want an error that names SMOKEALARM_KUBELET_CA_FILE", err)
			}
		})
	}
}

// The token must not follow a redirect to another address.
func TestKubeletClientFollowsNoRedirect(t *testing.T) {
	elsewhere := fakeKubelet(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the redirect was followed")
	})
	server := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/stats/summary", http.StatusFound)
	})

	_, reason := httpsCluster(t, kubeletClientFor(t, server)).collectVolumeUsage(context.Background(),
		[]*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if !strings.Contains(reason, "HTTP 302") {
		t.Errorf("reason %q; want the redirect reported as an error", reason)
	}
}

// One node that cannot be read skips the whole check, as with the proxy.
func TestKubeletOneOfTwoNodesFails(t *testing.T) {
	good := fakeKubelet(t, statsHandler(t))
	broken := fakeKubelet(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	// Both test servers share the certificate of the httptest package.
	cluster := httpsCluster(t, kubeletClientFor(t, good))

	usage, reason := cluster.collectVolumeUsage(context.Background(),
		[]*corev1.Node{nodeAt(t, "pi-1", good.URL), nodeAt(t, "pi-2", broken.URL)})
	if usage != nil || !strings.Contains(reason, "pi-2") || !strings.Contains(reason, "HTTP 500") {
		t.Errorf("usage %v, reason %q; want the check skipped and pi-2 named", usage, reason)
	}
}

func TestKubeletStatsURL(t *testing.T) {
	tests := []struct {
		name      string
		addresses []corev1.NodeAddress
		port      int32
		want      string
	}{
		{"internal IP and published port", []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "pi-1"},
			{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
		}, 10255, "https://10.0.0.11:10255/stats/summary"},
		{"no port published", []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}}, 0,
			"https://10.0.0.11:10250/stats/summary"},
		{"IPv6", []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "fd00::11"}}, 10250,
			"https://[fd00::11]:10250/stats/summary"},
		{"first internal IP wins", []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
			{Type: corev1.NodeInternalIP, Address: "fd00::11"},
		}, 10250, "https://10.0.0.11:10250/stats/summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{Status: corev1.NodeStatus{Addresses: tt.addresses}}
			node.Status.DaemonEndpoints.KubeletEndpoint.Port = tt.port
			got, err := kubeletStatsURL(node)
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestKubeletStatsURLWithoutInternalIP(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pi-1"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: "pi-1"}}},
	}
	_, err := kubeletStatsURL(node)
	if err == nil {
		t.Fatal("expected an error for a node without InternalIP")
	}
	if reason := skipReasonFor(node.Name, err); !strings.Contains(reason, "InternalIP") {
		t.Errorf("reason %q does not name the missing InternalIP", reason)
	}
}

// A kubelet that accepts the connection but answers slowly is not a network
// problem, so the reason must not send the user looking for a firewall rule.
func TestKubeletAnswersTooSlowly(t *testing.T) {
	server := fakeKubelet(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	cluster := httpsCluster(t, kubeletClientFor(t, server))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, reason := cluster.collectVolumeUsage(ctx, []*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if !strings.Contains(reason, "did not answer in time") || strings.Contains(reason, "NetworkPolicy") {
		t.Errorf("reason %q; want a slow answer, not an unreachable kubelet", reason)
	}
}

func TestKubeletAnswerTooLarge(t *testing.T) {
	server := fakeKubelet(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxStatsSize+1))
	})
	_, reason := httpsCluster(t, kubeletClientFor(t, server)).collectVolumeUsage(context.Background(),
		[]*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if !strings.Contains(reason, "larger than 32 MiB") {
		t.Errorf("reason %q; want the size named instead of a parse error", reason)
	}
}

// Of an error answer only the start is kept, however long it is.
func TestKubeletLongErrorAnswer(t *testing.T) {
	server := fakeKubelet(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("broken ", 1<<20))
	})
	_, reason := httpsCluster(t, kubeletClientFor(t, server)).collectVolumeUsage(context.Background(),
		[]*corev1.Node{nodeAt(t, "pi-1", server.URL)})
	if !strings.Contains(reason, "HTTP 500") || len(reason) > 2*excerptLength {
		t.Errorf("reason has %d bytes: %.300q; want HTTP 500 and a short excerpt", len(reason), reason)
	}
}

func TestExcerptCutsBetweenCharacters(t *testing.T) {
	// "ä" takes two bytes, so one of them falls on the limit.
	text := excerpt([]byte(strings.Repeat("a", excerptLength-1) + strings.Repeat("ä", 10)))
	if !utf8.ValidString(text) || !strings.HasSuffix(text, "…") || len(text) > excerptLength+len("…") {
		t.Errorf("excerpt = %q; want valid UTF-8, cut with an ellipsis", text)
	}
	if got := excerpt([]byte("  Forbidden\n  (user=x)  ")); got != "Forbidden (user=x)" {
		t.Errorf("excerpt = %q, want it on one line", got)
	}
}

func TestSkipReasons(t *testing.T) {
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	defer cert.Close()
	kubeletAddr := &net.TCPAddr{IP: net.ParseIP("10.0.0.11"), Port: 10250}
	readReset := func(localPort int) error {
		return &url.Error{Op: "Get", URL: "https://10.0.0.11:10250/stats/summary", Err: &net.OpError{
			Op: "read", Net: "tcp", Addr: kubeletAddr,
			Source: &net.TCPAddr{IP: net.ParseIP("10.42.0.7"), Port: localPort},
			Err:    errors.New("connection reset by peer"),
		}}
	}
	tests := []struct {
		name     string
		err      error
		want     []string
		unwanted []string
	}{
		{"certificate without the node IP", &url.Error{Op: "Get", URL: "https://10.0.0.11:10250/stats/summary",
			Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: cert.Certificate(), Host: "10.0.0.11"}}},
			[]string{"does not name the address", "10.0.0.11", "InternalIP"}, []string{"SMOKEALARM_KUBELET_CA_FILE"}},
		{"unknown CA", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			[]string{"could not verify", "SMOKEALARM_KUBELET_CA_FILE", "serverTLSBootstrap"}, nil},
		{"TLS alert from the kubelet", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")},
			[]string{"could not read the kubelet stats", "tls: handshake failure"}, []string{"could not reach"}},
		{"connection reset", readReset(51234),
			[]string{"read 10.0.0.11:10250: connection reset by peer"}, []string{"51234", "could not reach"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := skipReasonFor("pi-1", tt.err)
			for _, want := range tt.want {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not contain %q", reason, want)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(reason, unwanted) {
					t.Errorf("reason %q contains %q", reason, unwanted)
				}
			}
		})
	}

	// The engine logs a skip whenever its reason changes, so the same cause
	// must give the same text on every run.
	if first, second := skipReasonFor("pi-1", readReset(51234)), skipReasonFor("pi-1", readReset(51999)); first != second {
		t.Errorf("reasons differ by the local port:\n%s\n%s", first, second)
	}
}

// HTTPS_PROXY is meant for the notification targets; the kubelets are inside
// the cluster and must be called directly.
func TestKubeletConfigUsesNoProxy(t *testing.T) {
	original := &rest.Config{BearerToken: testToken}
	cfg, err := kubeletConfig(original, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://10.0.0.11:10250/stats/summary", nil)
	if cfg.Proxy == nil {
		t.Fatal("no proxy function: client-go would use HTTPS_PROXY")
	}
	if proxy, err := cfg.Proxy(req); proxy != nil || err != nil {
		t.Errorf("proxy = %v, %v; want none", proxy, err)
	}
	if original.Proxy != nil {
		t.Error("kubeletConfig changed the config of the API client")
	}
}
