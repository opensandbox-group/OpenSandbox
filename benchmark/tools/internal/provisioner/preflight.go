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

package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const preflightProbeContainerName = "probe"

// Preflight inspects cluster nodes via the K8s API and launches short-lived
// node probe pods to verify KVM, cgroup v2, ublk_drv, StateRoot reflink,
// spill directory availability, and CPU family/model/stepping homogeneity.
func (p *OverlayBDProvisioner) Preflight(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) (*report.EnvironmentFingerprint, error) {
	if prof == nil {
		return nil, fmt.Errorf("benchmark profile must not be nil")
	}
	if err := prof.Validate(); err != nil {
		return nil, fmt.Errorf("invalid benchmark profile: %w", err)
	}

	rc, err := p.resolveClients(opts)
	if err != nil {
		return nil, err
	}

	_, _, benchNS := resolveNamespaces(prof)
	if err := ensureNamespace(ctx, rc.kube, benchNS); err != nil {
		return nil, err
	}

	fp := &report.EnvironmentFingerprint{
		PoolMode: prof.Stack.FastSandbox.Pool.Mode,
		OverlayBD: map[string]any{
			"enabled":              prof.Stack.FastSandbox.Runtime.OCI.Enabled,
			"cache_size_gb":        prof.Stack.FastSandbox.Runtime.OCI.CacheSizeGB,
			"pool_low":             prof.Stack.FastSandbox.Runtime.OCI.PoolLow,
			"pool_high":            prof.Stack.FastSandbox.Runtime.OCI.PoolHigh,
			"pool_size_gb":         prof.Stack.FastSandbox.Runtime.OCI.PoolSizeGB,
			"spill_dir":            prof.Stack.FastSandbox.Pool.SpillDir,
			"blob_cache_max_bytes": prof.Stack.FastSandbox.Pool.BlobCacheMaxBytes,
		},
	}
	if prof.Suites.ScheduleBurst != nil && prof.Suites.ScheduleBurst.ReadinessPollMs > 0 {
		fp.ReadinessPollMs = prof.Suites.ScheduleBurst.ReadinessPollMs
	}

	if ver, err := rc.kube.Discovery().ServerVersion(); err == nil && ver != nil {
		fp.KubernetesVersion = ver.GitVersion
	}

	nodeList, err := rc.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list cluster nodes: %w", err)
	}
	candidates := selectCandidateNodes(nodeList.Items)
	if len(candidates) == 0 {
		return fp, fmt.Errorf("preflight failed: no Ready candidate nodes found in cluster")
	}

	fp.ClusterArch = normalizeArch(candidates[0].Status.NodeInfo.Architecture)
	spillDir := prof.Stack.FastSandbox.Pool.SpillDir
	probeImage := resolveProbeImage(prof)

	var fatalErrs []error
	cpuIdentities := make(map[string][]string)

	for _, node := range candidates {
		info := report.NodeInfo{
			Name:             node.Name,
			KernelVersion:    node.Status.NodeInfo.KernelVersion,
			OSImage:          node.Status.NodeInfo.OSImage,
			ContainerRuntime: node.Status.NodeInfo.ContainerRuntimeVersion,
			CPUCapacity:      node.Status.Capacity.Cpu().String(),
			MemoryCapacity:   node.Status.Capacity.Memory().String(),
		}

		probeOut, probeErr := p.runNodePreflightProbe(ctx, rc, benchNS, node.Name, probeImage, defaultStateRoot, spillDir)
		if probeErr != nil {
			fatalErrs = append(fatalErrs, fmt.Errorf("node %q preflight probe failed: %w", node.Name, probeErr))
			fp.Nodes = append(fp.Nodes, info)
			continue
		}

		info.KVMReady = probeOut.kvmReady
		info.UblkReady = probeOut.ublkReady
		info.StateRootFS = probeOut.stateRootFS
		info.ReflinkSupported = probeOut.reflinkSupported
		info.SpillDirAvailable = probeOut.spillDirAvailable
		info.CPUIdentity = probeOut.cpuIdentity
		if info.CPUIdentity != "" {
			cpuIdentities[info.CPUIdentity] = append(cpuIdentities[info.CPUIdentity], node.Name)
		}
		fp.Nodes = append(fp.Nodes, info)

		if !probeOut.kvmReady {
			fatalErrs = append(fatalErrs, fmt.Errorf("node %q missing /dev/kvm (KVM is required for fast-sandbox Firecracker)", node.Name))
		}
		if !probeOut.ublkReady {
			fatalErrs = append(fatalErrs, fmt.Errorf("node %q missing ublk_drv kernel module or /dev/ublk-control (required for OverlayBD OCI; run with --prepare-nodes or modprobe ublk_drv)", node.Name))
		}
		if !probeOut.cgroupV2 {
			rc.logger.Warn("node does not report cgroup v2 (/sys/fs/cgroup/cgroup.controllers missing); ResourceAware cgroup enforcement and PSI metrics may be degraded", "node", node.Name)
		}
		if (prof.Stack.FastSandbox.Pool.Mode == "" || prof.Stack.FastSandbox.Pool.Mode == profile.PoolModeResourceAware) && !probeOut.psiReady {
			rc.logger.Warn("node kernel does not expose /proc/pressure/memory (PSI disabled); ResourceAware fastlets will reject placement (boot kernel with psi=1 or set stack.fastSandbox.pool.mode=LegacySlot)", "node", node.Name)
		}
		if !probeOut.reflinkSupported {
			rc.logger.Warn("node StateRoot does not support cp --reflink=always; captureOCIUpper will degrade from O(1) reflink copy", "node", node.Name, "stateroot_fs", probeOut.stateRootFS)
		}
		if spillDir != "" && !probeOut.spillDirAvailable {
			rc.logger.Warn("configured FAST_SANDBOX_SNAPSHOT_SPILL_DIR is unavailable on node", "node", node.Name, "spill_dir", spillDir)
		}
	}

	if len(cpuIdentities) > 1 {
		rc.logger.Warn("heterogeneous host CPU identities detected across candidate nodes; cross-node Firecracker Full snapshot resume may fail",
			"identities", cpuIdentities)
	}

	if len(fatalErrs) > 0 {
		return fp, fmt.Errorf("preflight failed:\n%w", errors.Join(fatalErrs...))
	}
	return fp, nil
}

type nodeProbeResult struct {
	kvmReady          bool
	cgroupV2          bool
	psiReady          bool
	ublkReady         bool
	stateRootFS       string
	reflinkSupported  bool
	spillDirAvailable bool
	cpuIdentity       string
}

func (p *OverlayBDProvisioner) runNodePreflightProbe(
	ctx context.Context,
	rc *resolvedClients,
	namespace, nodeName, probeImage, stateRoot, spillDir string,
) (*nodeProbeResult, error) {
	podName := fmt.Sprintf("osb-preflight-%s", sanitizeDNSLabel(nodeName, 40))
	pod := buildPreflightProbePod(namespace, podName, nodeName, probeImage, stateRoot, spillDir)

	// Clean up any stale probe pod with the same name before creating.
	_ = rc.kube.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: int64Ptr(0)})

	if _, err := rc.kube.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create preflight pod %s/%s: %w", namespace, podName, err)
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := rc.kube.CoreV1().Pods(namespace).Delete(cleanCtx, podName, metav1.DeleteOptions{GracePeriodSeconds: int64Ptr(0)}); err != nil && !apierrors.IsNotFound(err) {
			rc.logger.Warn("failed to delete preflight probe pod", "namespace", namespace, "pod", podName, "error", err)
		}
	}()

	if err := p.waitForPodTerminal(ctx, rc.kube, namespace, podName); err != nil {
		return nil, err
	}

	logs, err := rc.logs.ReadLogs(ctx, rc.kube, namespace, podName, preflightProbeContainerName)
	if err != nil {
		return nil, err
	}
	return parseNodeProbeOutput(logs), nil
}

func buildPreflightProbePod(namespace, podName, nodeName, image, stateRoot, spillDir string) *corev1.Pod {
	script := fmt.Sprintf(`set -u
KVM_READY=false
if [ -c /host-dev/kvm ]; then KVM_READY=true; fi

CGROUP_V2=false
if [ -f /host-sys/fs/cgroup/cgroup.controllers ]; then CGROUP_V2=true; fi

PSI_READY=false
if [ -r /host-proc/pressure/memory ]; then PSI_READY=true; fi

UBLK_READY=false
if [ -c /host-dev/ublk-control ] || grep -qw ublk_drv /host-proc/modules 2>/dev/null; then UBLK_READY=true; fi

STATEROOT_FS=$(stat -f -c %%T /host-stateroot 2>/dev/null || echo unknown)

REFLINK_SUPPORTED=false
SRC=/host-stateroot/.osb-reflink-src-$$
DST=/host-stateroot/.osb-reflink-dst-$$
if touch "$SRC" 2>/dev/null && cp --reflink=always "$SRC" "$DST" 2>/dev/null; then
  REFLINK_SUPPORTED=true
fi
rm -f "$SRC" "$DST" 2>/dev/null || true

SPILL_DIR_AVAILABLE=false
if [ -n %q ] && [ -d /host-spill ] && [ -w /host-spill ]; then
  SPILL_DIR_AVAILABLE=true
fi

VENDOR=$(awk -F: '/^vendor_id/{gsub(/^[ \t]+/,"",$2); print $2; exit}' /host-proc/cpuinfo 2>/dev/null || true)
FAMILY=$(awk -F: '/^cpu family/{gsub(/^[ \t]+/,"",$2); print $2; exit}' /host-proc/cpuinfo 2>/dev/null || true)
MODEL=$(awk -F: '/^model[ \t]*:/{gsub(/^[ \t]+/,"",$2); print $2; exit}' /host-proc/cpuinfo 2>/dev/null || true)
STEPPING=$(awk -F: '/^stepping/{gsub(/^[ \t]+/,"",$2); print $2; exit}' /host-proc/cpuinfo 2>/dev/null || true)
CPU_IDENTITY="${VENDOR}:${FAMILY}:${MODEL}:${STEPPING}"

echo "KVM_READY=${KVM_READY}"
echo "CGROUP_V2=${CGROUP_V2}"
echo "PSI_READY=${PSI_READY}"
echo "UBLK_READY=${UBLK_READY}"
echo "STATEROOT_FS=${STATEROOT_FS}"
echo "REFLINK_SUPPORTED=${REFLINK_SUPPORTED}"
echo "SPILL_DIR_AVAILABLE=${SPILL_DIR_AVAILABLE}"
echo "CPU_IDENTITY=${CPU_IDENTITY}"
`, spillDir)

	dirType := corev1.HostPathDirectory
	dirOrCreate := corev1.HostPathDirectoryOrCreate

	volumes := []corev1.Volume{
		{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &dirType}}},
		{Name: "host-proc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc", Type: &dirType}}},
		{Name: "host-sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys", Type: &dirType}}},
		{Name: "host-stateroot", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: stateRoot, Type: &dirOrCreate}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "host-dev", MountPath: "/host-dev", ReadOnly: true},
		{Name: "host-proc", MountPath: "/host-proc", ReadOnly: true},
		{Name: "host-sys", MountPath: "/host-sys", ReadOnly: true},
		{Name: "host-stateroot", MountPath: "/host-stateroot"},
	}
	if spillDir != "" {
		volumes = append(volumes, corev1.Volume{
			Name:         "host-spill",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: spillDir, Type: &dirOrCreate}},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "host-spill",
			MountPath: "/host-spill",
		})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "opensandbox-bench-preflight",
				managedByLabelKey:        managedByLabelValue,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{
				{
					Name:    preflightProbeContainerName,
					Image:   image,
					Command: []string{"sh", "-c", script},
					SecurityContext: &corev1.SecurityContext{
						Privileged: boolPtr(true),
					},
					VolumeMounts: mounts,
				},
			},
			Volumes: volumes,
		},
	}
}

func parseNodeProbeOutput(raw string) *nodeProbeResult {
	res := &nodeProbeResult{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "KVM_READY":
			res.kvmReady = v == "true"
		case "CGROUP_V2":
			res.cgroupV2 = v == "true"
		case "PSI_READY":
			res.psiReady = v == "true"
		case "UBLK_READY":
			res.ublkReady = v == "true"
		case "STATEROOT_FS":
			res.stateRootFS = v
		case "REFLINK_SUPPORTED":
			res.reflinkSupported = v == "true"
		case "SPILL_DIR_AVAILABLE":
			res.spillDirAvailable = v == "true"
		case "CPU_IDENTITY":
			if v != ":::" {
				res.cpuIdentity = v
			}
		}
	}
	return res
}

func (p *OverlayBDProvisioner) waitForPodTerminal(ctx context.Context, client kubernetes.Interface, namespace, podName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		pod, err := client.CoreV1().Pods(namespace).Get(waitCtx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("poll pod %s/%s: %w", namespace, podName, err)
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s/%s failed", namespace, podName)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for pod %s/%s to complete: %w", namespace, podName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func sanitizeDNSLabel(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "node"
	}
	if maxLen > 0 && len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}
