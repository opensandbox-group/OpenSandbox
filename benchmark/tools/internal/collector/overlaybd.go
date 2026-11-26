// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const bytesPerGiB = 1024 * 1024 * 1024

// UblkdPoolStatus models the JSON response from overlaybd-ublkd GET /v1/pool.
type UblkdPoolStatus struct {
	Enabled      bool  `json:"enabled"`
	SizeGB       int   `json:"size_gb"`
	Low          int   `json:"low"`
	High         int   `json:"high"`
	Idle         int   `json:"idle"`
	InUse        int   `json:"in_use"`
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	TotalCreated int64 `json:"total_created,omitempty"`
}

// NodeStorageProbe holds StateRoot and OverlayBD cache sizing from a single runtime node.
type NodeStorageProbe struct {
	NodeName                 string  `json:"node_name"`
	StateRootUsedBytes       int64   `json:"stateroot_used_bytes"`
	StateRootFreeBytes       int64   `json:"stateroot_free_bytes"`
	StateRootFreeHeadroomGiB float64 `json:"stateroot_free_headroom_gib"`
	OverlayBDBlockCacheBytes int64   `json:"overlaybd_block_cache_bytes"`
	FSBBlobCacheBytes        int64   `json:"fsb_blob_cache_bytes"`
	FSBBlobCacheFileCount    int     `json:"fsb_blob_cache_file_count"`
}

// OverlayBDNodeSnapshot captures /v1/pool, daemon restart count, and storage probe for one node.
type OverlayBDNodeSnapshot struct {
	NodeName            string           `json:"node_name"`
	PodName             string           `json:"pod_name"`
	Pool                *UblkdPoolStatus `json:"pool,omitempty"`
	UblkdDaemonRestarts int              `json:"ublkd_daemon_restarts"`
	Storage             NodeStorageProbe `json:"storage"`
}

// OverlayBDSnapshot aggregates OverlayBD telemetry across all runtime nodes at a point in time.
type OverlayBDSnapshot struct {
	Timestamp time.Time                        `json:"timestamp"`
	Nodes     map[string]OverlayBDNodeSnapshot `json:"nodes"`
	Warnings  []string                         `json:"warnings,omitempty"`
}

// OverlayBDProbe queries overlaybd-ublkd /v1/pool and probes StateRoot / cache directories
// via firecracker-runtime DaemonSet pods.
type OverlayBDProbe struct {
	cfg     Config
	clients *Clients
}

// NewOverlayBDProbe constructs an OverlayBDProbe.
func NewOverlayBDProbe(cfg Config, clients *Clients) *OverlayBDProbe {
	cfg.applyDefaults()
	return &OverlayBDProbe{cfg: cfg, clients: clients}
}

// Snapshot collects /v1/pool, overlaybd-ublkd restarts, and StateRoot/cache probes across all runtime nodes.
func (p *OverlayBDProbe) Snapshot(ctx context.Context) (*OverlayBDSnapshot, error) {
	snap := &OverlayBDSnapshot{
		Timestamp: time.Now().UTC(),
		Nodes:     make(map[string]OverlayBDNodeSnapshot),
	}
	pods, err := listFirecrackerRuntimePods(ctx, p.clients.Clientset, p.cfg.SystemNamespace, p.cfg.ResourceNamespace)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		pod := &pods[i]
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			nodeName = pod.Name
		}
		ns := OverlayBDNodeSnapshot{
			NodeName:            nodeName,
			PodName:             pod.Name,
			UblkdDaemonRestarts: ublkdRestartCount(pod),
		}
		pool, poolErr := p.QueryPoolStatus(ctx, pod)
		if poolErr != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("node %s /v1/pool: %v", nodeName, poolErr))
		} else {
			ns.Pool = pool
		}
		storage, storErr := p.ProbeNodeStorage(ctx, pod)
		if storErr != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("node %s storage probe: %v", nodeName, storErr))
		} else {
			ns.Storage = storage
		}
		snap.Nodes[nodeName] = ns
	}
	return snap, nil
}

// QueryPoolStatus queries overlaybd-ublkd GET /v1/pool over its Unix domain socket inside the runtime pod.
func (p *OverlayBDProbe) QueryPoolStatus(ctx context.Context, pod *corev1.Pod) (*UblkdPoolStatus, error) {
	container := pickRuntimeContainer(pod, DefaultUblkdContainer, DefaultFirecrackerAgentContainer, DefaultJanitorContainer)
	cmd := []string{
		"curl", "-fsS", "--max-time", "3",
		"--unix-socket", p.cfg.UblkdSocketPath,
		"http://localhost/v1/pool",
	}
	stdout, stderr, err := execCommand(ctx, p.clients.Execer, pod.Namespace, pod.Name, container, cmd, p.cfg.ExecTimeout)
	if err != nil {
		return nil, fmt.Errorf("exec ublkd /v1/pool on %s/%s: %w (stderr: %s)", pod.Namespace, pod.Name, err, strings.TrimSpace(stderr))
	}
	return ParseUblkdPoolStatus(stdout)
}

// ParseUblkdPoolStatus decodes JSON returned by overlaybd-ublkd /v1/pool.
func ParseUblkdPoolStatus(raw string) (*UblkdPoolStatus, error) {
	var st UblkdPoolStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &st); err != nil {
		return nil, fmt.Errorf("parse /v1/pool json: %w", err)
	}
	return &st, nil
}

// ProbeNodeStorage inspects StateRoot disk usage/free headroom and both OverlayBD caches
// (overlaybd-ublkd block cache and fsb-vm oci/blobs/sha256 blob cache).
func (p *OverlayBDProbe) ProbeNodeStorage(ctx context.Context, pod *corev1.Pod) (NodeStorageProbe, error) {
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		nodeName = pod.Name
	}
	container := pickRuntimeContainer(pod, DefaultUblkdContainer, DefaultFirecrackerAgentContainer, DefaultJanitorContainer)
	sr := p.cfg.StateRoot

	// Single lightweight shell invocation returning key=value lines so we avoid multiple exec round-trips.
	script := fmt.Sprintf(`
SR=%q
FREE=$(df -B1 "$SR" 2>/dev/null | awk 'NR==2 {print $4}')
USED=$(du -sb "$SR" 2>/dev/null | awk '{print $1}')
OBD_CACHE=0
for d in "$SR/overlaybd-cache" "$SR/cache" "$SR/oci/cache" /var/cache/overlaybd; do
  if [ -d "$d" ]; then
    OBD_CACHE=$(du -sb "$d" 2>/dev/null | awk '{print $1}')
    break
  fi
done
BLOB_DIR="$SR/oci/blobs/sha256"
BLOB_BYTES=0
BLOB_FILES=0
if [ -d "$BLOB_DIR" ]; then
  BLOB_BYTES=$(du -sb "$BLOB_DIR" 2>/dev/null | awk '{print $1}')
  BLOB_FILES=$(find "$BLOB_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')
fi
echo "stateroot_free_bytes=${FREE:-0}"
echo "stateroot_used_bytes=${USED:-0}"
echo "overlaybd_block_cache_bytes=${OBD_CACHE:-0}"
echo "fsb_blob_cache_bytes=${BLOB_BYTES:-0}"
echo "fsb_blob_cache_files=${BLOB_FILES:-0}"
`, sr)

	stdout, stderr, err := execCommand(ctx, p.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", script}, p.cfg.ExecTimeout)
	if err != nil {
		return NodeStorageProbe{NodeName: nodeName}, fmt.Errorf("storage probe on %s/%s: %w (stderr: %s)", pod.Namespace, pod.Name, err, strings.TrimSpace(stderr))
	}
	return ParseNodeStorageOutput(nodeName, stdout), nil
}

// ParseNodeStorageOutput parses the key=value output produced by ProbeNodeStorage.
func ParseNodeStorageOutput(nodeName, raw string) NodeStorageProbe {
	probe := NodeStorageProbe{NodeName: nodeName}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "stateroot_free_bytes":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				probe.StateRootFreeBytes = n
				probe.StateRootFreeHeadroomGiB = float64(n) / float64(bytesPerGiB)
			}
		case "stateroot_used_bytes":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				probe.StateRootUsedBytes = n
			}
		case "overlaybd_block_cache_bytes":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				probe.OverlayBDBlockCacheBytes = n
			}
		case "fsb_blob_cache_bytes":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				probe.FSBBlobCacheBytes = n
			}
		case "fsb_blob_cache_files":
			if n, err := strconv.Atoi(v); err == nil {
				probe.FSBBlobCacheFileCount = n
			}
		}
	}
	return probe
}

func ublkdRestartCount(pod *corev1.Pod) int {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == DefaultUblkdContainer {
			return int(cs.RestartCount)
		}
	}
	return 0
}

// ComputeOverlayBDMetrics diffs before/after OverlayBDSnapshots into report.OverlayBDMetrics.
// If observedIdleMin < 0, idleMin is derived from the before/after snapshots.
func ComputeOverlayBDMetrics(before, after *OverlayBDSnapshot, observedIdleMin int, activeSandboxes int) *report.OverlayBDMetrics {
	if after == nil {
		return nil
	}
	var hitsDelta, missesDelta int64
	var idleEnd, restartsDelta int
	var totalUsedBytesDelta int64
	minFreeGiB := math.Inf(1)
	minIdle := math.MaxInt

	for node, afterNode := range after.Nodes {
		var beforeNode OverlayBDNodeSnapshot
		hasBefore := false
		if before != nil {
			beforeNode, hasBefore = before.Nodes[node]
		}
		if afterNode.Pool != nil {
			idleEnd += afterNode.Pool.Idle
			if afterNode.Pool.Idle < minIdle {
				minIdle = afterNode.Pool.Idle
			}
			var bHits, bMisses int64
			if hasBefore && beforeNode.Pool != nil {
				bHits = beforeNode.Pool.Hits
				bMisses = beforeNode.Pool.Misses
				if beforeNode.Pool.Idle < minIdle {
					minIdle = beforeNode.Pool.Idle
				}
			}
			hd := afterNode.Pool.Hits - bHits
			if hd < 0 {
				hd = afterNode.Pool.Hits
			}
			md := afterNode.Pool.Misses - bMisses
			if md < 0 {
				md = afterNode.Pool.Misses
			}
			hitsDelta += hd
			missesDelta += md
		}
		rd := afterNode.UblkdDaemonRestarts
		if hasBefore {
			rd -= beforeNode.UblkdDaemonRestarts
			if rd < 0 {
				rd = afterNode.UblkdDaemonRestarts
			}
		}
		restartsDelta += rd

		if afterNode.Storage.StateRootFreeBytes > 0 {
			if afterNode.Storage.StateRootFreeHeadroomGiB < minFreeGiB {
				minFreeGiB = afterNode.Storage.StateRootFreeHeadroomGiB
			}
		}
		if afterNode.Storage.StateRootUsedBytes > 0 {
			usedDelta := afterNode.Storage.StateRootUsedBytes
			if hasBefore && beforeNode.Storage.StateRootUsedBytes > 0 {
				usedDelta -= beforeNode.Storage.StateRootUsedBytes
			}
			if usedDelta > 0 {
				totalUsedBytesDelta += usedDelta
			}
		}
	}

	var hitRate float64
	if hitsDelta+missesDelta > 0 {
		hitRate = float64(hitsDelta) / float64(hitsDelta+missesDelta)
	}
	if observedIdleMin >= 0 {
		minIdle = observedIdleMin
	} else if minIdle == math.MaxInt {
		minIdle = 0
	}
	if math.IsInf(minFreeGiB, 1) {
		minFreeGiB = 0
	}

	var duPerSbx int64
	if activeSandboxes > 0 && totalUsedBytesDelta > 0 {
		duPerSbx = totalUsedBytesDelta / int64(activeSandboxes)
	}

	return &report.OverlayBDMetrics{
		UblkdPoolHitsDelta:      hitsDelta,
		UblkdPoolMissesDelta:    missesDelta,
		UblkdPoolHitRate:        hitRate,
		UblkdPoolIdleMin:        minIdle,
		UblkdPoolIdleEnd:        idleEnd,
		UblkdDaemonRestarts:     restartsDelta,
		StateRootDuBytesPerSbx:  duPerSbx,
		StateRootFreeHeadroomGi: minFreeGiB,
	}
}
