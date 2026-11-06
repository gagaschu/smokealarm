package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

// statsTimeout limits how long one kubelet may take to answer.
const statsTimeout = 10 * time.Second

// defaultKubeletPort is used when a node does not say on which port its
// kubelet listens. It is the kubelet's own default.
const defaultKubeletPort = 10250

// maxStatsSize limits how much of a kubelet answer is read. A summary of a
// busy node is a few megabytes; anything far larger is not a stats summary.
const maxStatsSize = 32 << 20

// maxErrorAnswer is how much of an answer that is not 2xx is read. Only its
// start is shown, so there is no need to hold more in memory.
const maxErrorAnswer = 4 << 10

// excerptLength is how many bytes of an answer the status page shows.
const excerptLength = 200

// statsFetcher reads the raw stats summary of one node.
type statsFetcher func(ctx context.Context, node *corev1.Node) ([]byte, error)

// kubeletStatusError is an answer of the kubelet that is not 2xx.
type kubeletStatusError struct {
	Code    int
	Excerpt string // the start of the answer, for the log and the status page
}

func (e *kubeletStatusError) Error() string {
	if e.Excerpt == "" {
		return fmt.Sprintf("kubelet answered HTTP %d", e.Code)
	}
	return fmt.Sprintf("kubelet answered HTTP %d: %s", e.Code, e.Excerpt)
}

// NewKubeletClient builds the HTTP client that calls the kubelets directly.
// It uses the same credentials as the API client: in the cluster the
// ServiceAccount token (client-go reads it again when it is rotated), locally
// the kubeconfig. The kubelet checks the token itself and allows
// /stats/summary with "get" on nodes/stats, which grants only resource
// statistics. Going through the API server instead would need nodes/proxy,
// which opens the whole kubelet API (exec, logs).
//
// The kubelet's certificate is always verified: against caFile if set,
// otherwise against the CA of the API client (the ServiceAccount ca.crt).
func NewKubeletClient(cfg *rest.Config, caFile string) (*http.Client, error) {
	kubeletCfg, err := kubeletConfig(cfg, caFile)
	if err != nil {
		return nil, err
	}
	transport, err := rest.TransportFor(kubeletCfg)
	if err != nil {
		return nil, fmt.Errorf("create kubelet transport: %w", err)
	}
	return &http.Client{
		Transport: transport,
		// The token must not follow a redirect to another address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// kubeletConfig derives the settings for the kubelets from those of the API
// client. The original is not changed.
func kubeletConfig(cfg *rest.Config, caFile string) (*rest.Config, error) {
	kubeletCfg := rest.CopyConfig(cfg)
	// A kubeconfig that skips verification for the API server must not do so
	// for the kubelets, and the API server's name is not the node's name.
	kubeletCfg.Insecure = false
	kubeletCfg.ServerName = ""
	// The kubelets are inside the cluster network. HTTPS_PROXY (set for the
	// notification targets) must not apply to them: a proxy outside would
	// either fail or see the token.
	kubeletCfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read SMOKEALARM_KUBELET_CA_FILE: %w", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("SMOKEALARM_KUBELET_CA_FILE: %s holds no PEM certificate", caFile)
		}
		kubeletCfg.CAFile = ""
		kubeletCfg.CAData = pem
	}
	return kubeletCfg, nil
}

// kubeletStatsFetcher asks the kubelet of a node for its stats summary:
// GET https://<InternalIP>:<kubelet port>/stats/summary.
func kubeletStatsFetcher(client *http.Client) statsFetcher {
	return func(ctx context.Context, node *corev1.Node) ([]byte, error) {
		statsURL, err := kubeletStatsURL(node)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build kubelet request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		return readStatsAnswer(resp)
	}
}

// readStatsAnswer returns the body of a 2xx answer, or an error that holds the
// start of any other answer.
func readStatsAnswer(resp *http.Response) ([]byte, error) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		start, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorAnswer))
		if err != nil {
			return nil, fmt.Errorf("kubelet answered HTTP %d, read the answer: %w", resp.StatusCode, err)
		}
		return nil, &kubeletStatusError{Code: resp.StatusCode, Excerpt: excerpt(start)}
	}
	// One byte more than allowed tells a full answer from one that was cut.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStatsSize+1))
	if err != nil {
		return nil, fmt.Errorf("read kubelet answer: %w", err)
	}
	if len(data) > maxStatsSize {
		return nil, fmt.Errorf("the kubelet answer is larger than %d MiB", maxStatsSize>>20)
	}
	return data, nil
}

// kubeletStatsURL builds the address of a node's stats summary from its
// InternalIP and the kubelet port it publishes.
func kubeletStatsURL(node *corev1.Node) (string, error) {
	ip := ""
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			ip = address.Address
			break
		}
	}
	if ip == "" {
		return "", errors.New("the node has no InternalIP address")
	}
	port := int(node.Status.DaemonEndpoints.KubeletEndpoint.Port)
	if port == 0 {
		port = defaultKubeletPort
	}
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/stats/summary", nil
}

// excerpt keeps the start of an answer on one line, short enough for the
// status page, and cuts it between two characters, never inside one.
func excerpt(data []byte) string {
	text := strings.Join(strings.Fields(string(data)), " ")
	if len(text) <= excerptLength {
		return text
	}
	end := excerptLength
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

// skipReasonFor explains why the stats are unavailable, with a hint for each
// of the usual causes: the permission, the certificate and the network.
//
// The text must stay the same while the cause does: the engine logs a skip
// again whenever its reason changes. So the errors are described without the
// parts that differ on every try, such as the local port of a connection.
func skipReasonFor(node string, err error) string {
	var status *kubeletStatusError
	if errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden) {
		return fmt.Sprintf("the kubelet of node %s refused the request (%v): smokealarm needs get on nodes/stats, "+
			"and the kubelet must use webhook authentication and authorization; grant it, "+
			"or disable the check with SMOKEALARM_DISABLED_CHECKS=pvc-usage", node, status)
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return fmt.Sprintf("the TLS certificate of the kubelet on node %s does not name the address smokealarm calls: %v; "+
			"the kubelet serving certificate must contain the node's InternalIP", node, hostErr)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return fmt.Sprintf("could not verify the TLS certificate of the kubelet on node %s: %v; "+
			"if the kubelet serving certificates are signed by another CA, set SMOKEALARM_KUBELET_CA_FILE "+
			"(Helm: kubelet.caConfigMap); self-signed ones (kubeadm without serverTLSBootstrap) cannot be verified",
			node, certErr.Err)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) && netErr.Op == "dial" {
		return fmt.Sprintf("could not reach the kubelet of node %s: %s; its port must be reachable "+
			"from the smokealarm pod (NetworkPolicy, firewall)", node, describeNetError(netErr))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("the kubelet of node %s did not answer in time (limit %s)", node, statsTimeout)
	}
	if errors.As(err, &netErr) {
		return fmt.Sprintf("could not read the kubelet stats of node %s: %s", node, describeNetError(netErr))
	}
	return fmt.Sprintf("could not read the kubelet stats of node %s: %v", node, err)
}

// describeNetError is net.OpError's own text without the local address,
// whose port changes with every connection.
func describeNetError(err *net.OpError) string {
	if err.Addr == nil {
		return fmt.Sprintf("%s: %v", err.Op, err.Err)
	}
	return fmt.Sprintf("%s %s: %v", err.Op, err.Addr, err.Err)
}
