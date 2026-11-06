package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// addVolumeUsage puts the volume usage into the snapshot. If the stats cannot
// be read, the check is marked as skipped instead.
func (c *Cluster) addVolumeUsage(ctx context.Context, snap *checks.Snapshot) {
	if c.fetchStats == nil {
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
