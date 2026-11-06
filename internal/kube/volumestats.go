package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// statsTimeout limits how long one kubelet may take to answer.
const statsTimeout = 10 * time.Second

// defaultKubeletPort is used when a node does not say on which port its
// kubelet listens. It is the kubelet's own default.
const defaultKubeletPort = 10250

// maxStatsSize limits how much of a kubelet answer is read. A summary of a
// busy node is a few megabytes; anything far larger is not a stats summary.
const maxStatsSize = 32 << 20

// statsFetcher reads the raw stats summary of one node.
type statsFetcher func(ctx context.Context, node *corev1.Node) ([]byte, error)

// errNoInternalIP means the node does not publish an address we can call.
var errNoInternalIP = errors.New("the node has no InternalIP address")

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
	kubeletCfg := rest.CopyConfig(cfg)
	// A kubeconfig that skips verification for the API server must not do so
	// for the kubelets, and the API server's name is not the node's name.
	kubeletCfg.Insecure = false
	kubeletCfg.ServerName = ""
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

		data, err := io.ReadAll(io.LimitReader(resp.Body, maxStatsSize))
		if err != nil {
			return nil, fmt.Errorf("read kubelet answer: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &kubeletStatusError{Code: resp.StatusCode, Excerpt: excerpt(data)}
		}
		return data, nil
	}
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
		return "", errNoInternalIP
	}
	port := int(node.Status.DaemonEndpoints.KubeletEndpoint.Port)
	if port == 0 {
		port = defaultKubeletPort
	}
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/stats/summary", nil
}

// excerpt keeps the start of an answer on one line, short enough for the
// status page.
func excerpt(data []byte) string {
	text := strings.Join(strings.Fields(string(data)), " ")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// addVolumeUsage puts the volume usage into the snapshot. If the stats cannot
// be read, the check is marked as skipped instead.
func (c *Cluster) addVolumeUsage(ctx context.Context, snap *checks.Snapshot) {
	if !c.volumeStats {
		return
	}
	usage, skipReason := c.collectVolumeUsage(ctx, snap.Nodes)
	if skipReason != "" {
		snap.Skipped = map[string]string{checks.PVCUsageCheck: skipReason}
		return
	}
	snap.VolumeUsage = usage
	snap.VolumeUsagePartial = slices.ContainsFunc(snap.Nodes, func(n *corev1.Node) bool { return !nodeIsReady(n) })
}

// collectVolumeUsage reads the stats of every Ready node. If any node fails,
// it gives up and returns a reason: with a node missing, its full volumes
// would look "fine again" and their problems would be resolved by mistake.
//
// Nodes that are not Ready are not asked at all. A dead node must not switch
// the check off for the whole cluster; node-not-ready reports it anyway. The
// problems of volumes that were not measured then stay as they are (see
// checks.VolumeUsageUnknown).
func (c *Cluster) collectVolumeUsage(ctx context.Context, nodes []*corev1.Node) (map[string]checks.VolumeUsage, string) {
	usage := map[string]checks.VolumeUsage{}
	for _, node := range nodes {
		if !nodeIsReady(node) {
			continue
		}
		nodeUsage, err := c.nodeVolumeUsage(ctx, node)
		if err != nil {
			return nil, skipReasonFor(node.Name, err)
		}
		for key, u := range nodeUsage {
			// A claim used by pods on several nodes keeps its highest usage.
			if existing, found := usage[key]; !found || u.UsedBytes > existing.UsedBytes {
				usage[key] = u
			}
		}
	}
	return usage, ""
}

func (c *Cluster) nodeVolumeUsage(ctx context.Context, node *corev1.Node) (map[string]checks.VolumeUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()

	data, err := c.fetchStats(ctx, node)
	if err != nil {
		return nil, err
	}
	return parseVolumeUsage(data)
}

// skipReasonFor explains why the stats are unavailable, with a hint for each
// of the usual causes: the permission, the certificate and the network.
func skipReasonFor(node string, err error) string {
	var status *kubeletStatusError
	if errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden) {
		return fmt.Sprintf("the kubelet of node %s refused the request (HTTP %d): smokealarm needs get on nodes/stats, "+
			"and the kubelet must use webhook authentication and authorization; grant it, "+
			"or disable the check with SMOKEALARM_DISABLED_CHECKS=pvc-usage", node, status.Code)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return fmt.Sprintf("could not verify the TLS certificate of the kubelet on node %s: %v; "+
			"if the kubelet serving certificates are signed by another CA, set SMOKEALARM_KUBELET_CA_FILE "+
			"(Helm: kubelet.caConfigMap)", node, certErr.Err)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("could not reach the kubelet of node %s: %v; its port must be reachable "+
			"from the smokealarm pod (NetworkPolicy, firewall)", node, err)
	}
	return fmt.Sprintf("could not read the kubelet stats of node %s: %v", node, err)
}

// parseVolumeUsage extracts the usage of persistent volume claims from a
// kubelet stats summary. Volumes that are not claims (emptyDir, ConfigMap,
// Secret) have no pvcRef and are ignored.
func parseVolumeUsage(data []byte) (map[string]checks.VolumeUsage, error) {
	var summary struct {
		Pods []struct {
			Volume []struct {
				UsedBytes     *int64 `json:"usedBytes"`
				CapacityBytes *int64 `json:"capacityBytes"`
				PVCRef        *struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pvcRef"`
			} `json:"volume"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("parse kubelet stats: %w", err)
	}

	usage := map[string]checks.VolumeUsage{}
	for _, pod := range summary.Pods {
		for _, volume := range pod.Volume {
			if volume.PVCRef == nil || volume.UsedBytes == nil || volume.CapacityBytes == nil {
				continue
			}
			key := volume.PVCRef.Namespace + "/" + volume.PVCRef.Name
			// The same claim can be mounted by several pods on this node.
			if existing, found := usage[key]; found && existing.UsedBytes >= *volume.UsedBytes {
				continue
			}
			usage[key] = checks.VolumeUsage{UsedBytes: *volume.UsedBytes, CapacityBytes: *volume.CapacityBytes}
		}
	}
	return usage, nil
}

// nodeIsReady reports whether the node's Ready condition is true.
func nodeIsReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
