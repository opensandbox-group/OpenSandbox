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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// IsolationTaintKey is temporarily applied to a source node during cross-node resume/restore
	// benchmarks so deleted fastlet pods do not reschedule back onto the source node before RestoreFastletNode.
	IsolationTaintKey   = "bench.opensandbox.io/isolated"
	IsolationTaintValue = "true"
)

// EvictCacheOptions configures OverlayBD cache eviction and ublk warm-pool re-warming.
type EvictCacheOptions struct {
	// NodeName optionally restricts eviction to a single runtime node; empty evicts on all runtime nodes.
	NodeName string
	// PoolLow is the target minimum idle count in /v1/pool before returning (defaults to Config.PoolLow).
	PoolLow int
	// RewarmTimeout bounds how long EvictOverlayBDCaches waits for /v1/pool idle >= PoolLow.
	RewarmTimeout time.Duration
	// PollInterval sets the polling cadence while waiting for /v1/pool re-warm.
	PollInterval time.Duration
}

// NodeIsolationState records the changes made by IsolateFastletNode so RestoreFastletNode can undo them cleanly.
type NodeIsolationState struct {
	NodeName       string
	TaintAdded     bool
	DeletedPods    []string
	Namespace      string
}

// ClusterOps provides benchmark-time cluster manipulation helpers (cache eviction,
// deterministic fastlet node isolation for cross-node tests, and ephemeral registry GC).
type ClusterOps struct {
	cfg     Config
	clients *Clients
	obd     *OverlayBDProbe
}

// NewClusterOps creates a ClusterOps helper.
func NewClusterOps(cfg Config, clients *Clients) *ClusterOps {
	cfg.applyDefaults()
	return &ClusterOps{
		cfg:     cfg,
		clients: clients,
		obd:     NewOverlayBDProbe(cfg, clients),
	}
}

// EvictOverlayBDCaches evicts fsb-vm blob-cache, manifest-cache, shared_snapfiles, and
// overlaybd-ublkd block cache on runtime nodes, then waits for /v1/pool to re-warm to poolLow
// so ublk device cold-create is not conflated with storage cache misses (OSEP-0034 §3).
func (c *ClusterOps) EvictOverlayBDCaches(ctx context.Context, opts EvictCacheOptions) error {
	if c.clients == nil || c.clients.Clientset == nil || c.clients.Execer == nil {
		return fmt.Errorf("EvictOverlayBDCaches requires kubernetes clientset and pod execer")
	}
	poolLow := opts.PoolLow
	if poolLow <= 0 {
		poolLow = c.cfg.PoolLow
	}
	rewarmTimeout := opts.RewarmTimeout
	if rewarmTimeout <= 0 {
		rewarmTimeout = 2 * time.Minute
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}

	pods, err := listFirecrackerRuntimePods(ctx, c.clients.Clientset, c.cfg.SystemNamespace, c.cfg.ResourceNamespace)
	if err != nil {
		return err
	}
	var targetPods []corev1.Pod
	for _, p := range pods {
		if opts.NodeName != "" && p.Spec.NodeName != opts.NodeName {
			continue
		}
		targetPods = append(targetPods, p)
	}
	if len(targetPods) == 0 {
		return fmt.Errorf("no running firecracker-runtime pods found (node filter=%q)", opts.NodeName)
	}

	sr := c.cfg.StateRoot
	evictScript := fmt.Sprintf(`
set -e
SR=%q
for d in \
  "$SR/oci/blobs/sha256" \
  "$SR/oci/manifests" \
  "$SR/manifest-cache" \
  "$SR/shared_snapfiles" \
  "$SR/oci/shared_snapfiles" \
  "$SR/overlaybd-cache" \
  "$SR/cache" \
  "$SR/oci/cache" \
  /var/cache/overlaybd; do
  if [ -d "$d" ]; then
    find "$d" -mindepth 1 -delete 2>/dev/null || rm -rf "$d"/* 2>/dev/null || true
  fi
done
`, sr)

	for i := range targetPods {
		pod := &targetPods[i]
		container := pickRuntimeContainer(pod, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
		_, stderr, execErr := execCommand(ctx, c.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", evictScript}, c.cfg.ExecTimeout)
		if execErr != nil {
			return fmt.Errorf("evict caches on node %s (%s/%s): %w (stderr: %s)", pod.Spec.NodeName, pod.Namespace, pod.Name, execErr, strings.TrimSpace(stderr))
		}
	}

	if poolLow <= 0 {
		return nil
	}

	// Wait until /v1/pool on every target node reaches idle >= poolLow.
	waitCtx, cancel := context.WithTimeout(ctx, rewarmTimeout)
	defer cancel()
	for {
		allWarm := true
		for i := range targetPods {
			pod := &targetPods[i]
			st, qErr := c.obd.QueryPoolStatus(waitCtx, pod)
			if qErr != nil || st == nil || st.Idle < poolLow {
				allWarm = false
				break
			}
		}
		if allWarm {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for overlaybd-ublkd /v1/pool idle >= %d: %w", poolLow, waitCtx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// IsolateFastletNode isolates a source node's fastlet from FastPath placement for deterministic
// cross-node resume/restore benchmarks (OSEP-0034 §3: kubectl cordon is ignored by FastPath).
// It adds a temporary NoSchedule taint on nodeName and deletes running fastlet Pod(s) on that node
// so FastPath removes the node from its candidate registry until RestoreFastletNode is called.
func (c *ClusterOps) IsolateFastletNode(ctx context.Context, nodeName string) (*NodeIsolationState, error) {
	if c.clients == nil || c.clients.Clientset == nil {
		return nil, fmt.Errorf("IsolateFastletNode requires kubernetes clientset")
	}
	if strings.TrimSpace(nodeName) == "" {
		return nil, fmt.Errorf("nodeName must be non-empty")
	}

	state := &NodeIsolationState{
		NodeName:  nodeName,
		Namespace: c.cfg.ResourceNamespace,
	}

	node, err := c.clients.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %q: %w", nodeName, err)
	}

	hasTaint := false
	for _, t := range node.Spec.Taints {
		if t.Key == IsolationTaintKey && t.Effect == corev1.TaintEffectNoSchedule {
			hasTaint = true
			break
		}
	}
	if !hasTaint {
		newTaints := append([]corev1.Taint(nil), node.Spec.Taints...)
		newTaints = append(newTaints, corev1.Taint{
			Key:    IsolationTaintKey,
			Value:  IsolationTaintValue,
			Effect: corev1.TaintEffectNoSchedule,
		})
		if err := c.patchNodeTaints(ctx, nodeName, newTaints); err != nil {
			return nil, fmt.Errorf("taint node %q: %w", nodeName, err)
		}
		state.TaintAdded = true
	}

	podList, err := c.clients.Clientset.CoreV1().Pods(c.cfg.ResourceNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return state, fmt.Errorf("list pods on node %q: %w", nodeName, err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName || !isFastletPod(pod) || pod.DeletionTimestamp != nil {
			continue
		}
		if err := c.clients.Clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return state, fmt.Errorf("delete fastlet pod %s/%s on node %q: %w", pod.Namespace, pod.Name, nodeName, err)
		}
		state.DeletedPods = append(state.DeletedPods, pod.Name)
	}
	return state, nil
}

// RestoreFastletNode removes the temporary isolation taint applied by IsolateFastletNode
// so the SandboxPool controller can restore fastlet capacity on the node.
func (c *ClusterOps) RestoreFastletNode(ctx context.Context, nodeName string) error {
	if c.clients == nil || c.clients.Clientset == nil {
		return fmt.Errorf("RestoreFastletNode requires kubernetes clientset")
	}
	if strings.TrimSpace(nodeName) == "" {
		return fmt.Errorf("nodeName must be non-empty")
	}

	node, err := c.clients.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %q: %w", nodeName, err)
	}

	var kept []corev1.Taint
	removed := false
	for _, t := range node.Spec.Taints {
		if t.Key == IsolationTaintKey && t.Effect == corev1.TaintEffectNoSchedule {
			removed = true
			continue
		}
		kept = append(kept, t)
	}
	if !removed {
		return nil
	}
	if err := c.patchNodeTaints(ctx, nodeName, kept); err != nil {
		return fmt.Errorf("remove isolation taint from node %q: %w", nodeName, err)
	}
	return nil
}

func (c *ClusterOps) patchNodeTaints(ctx context.Context, nodeName string, taints []corev1.Taint) error {
	patchBody := map[string]any{
		"spec": map[string]any{
			"taints": taints,
		},
	}
	raw, err := json.Marshal(patchBody)
	if err != nil {
		return err
	}
	_, err = c.clients.Clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, raw, metav1.PatchOptions{})
	return err
}

// TriggerRegistryGC runs `registry garbage-collect` inside the ephemeral in-cluster registry:2 Pod
// in benchNamespace between major benchmark suites (OSEP-0034 §2).
func (c *ClusterOps) TriggerRegistryGC(ctx context.Context, benchNamespace string) error {
	if c.clients == nil || c.clients.Clientset == nil || c.clients.Execer == nil {
		return fmt.Errorf("TriggerRegistryGC requires kubernetes clientset and pod execer")
	}
	if benchNamespace == "" {
		benchNamespace = c.cfg.BenchNamespace
	}

	podList, err := c.clients.Clientset.CoreV1().Pods(benchNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods in %q for registry GC: %w", benchNamespace, err)
	}
	var regPod *corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if podMatchesKeywords(p, "registry") {
			regPod = p
			break
		}
	}
	if regPod == nil {
		return fmt.Errorf("no running registry pod found in namespace %q", benchNamespace)
	}

	container := pickRuntimeContainer(regPod, "registry")
	cmd := []string{"registry", "garbage-collect", "--delete-untagged", "/etc/docker/registry/config.yml"}
	_, stderr, err := execCommand(ctx, c.clients.Execer, regPod.Namespace, regPod.Name, container, cmd, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("registry garbage-collect in %s/%s: %w (stderr: %s)", regPod.Namespace, regPod.Name, err, strings.TrimSpace(stderr))
	}
	return nil
}
