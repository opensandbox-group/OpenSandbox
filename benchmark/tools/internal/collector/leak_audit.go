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
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// LeakAuditOptions controls thresholds and baseline OOM counts for post-run leak auditing.
type LeakAuditOptions struct {
	MinStateRootFreeGiB     float64
	BaselineHostOOMKills    map[string]int64 // optional per-node /proc/vmstat oom_kill baseline
	BaselineCgroupOOMKills  int64            // optional fastlet podcgroup oom_kill baseline
	CRLabelSelector         string           // optional label selector scoping audited CRs
}

// LeakAuditMeasurements holds raw observed counts before pass/fail gate evaluation.
type LeakAuditMeasurements struct {
	HostOOMKills           int
	PodCgroupOOMKills      int
	ResidualSandboxCRs     int
	ResidualSnapshotCRs    int
	StuckDeletingSlots     int
	OrphanedUblkDevices    int
	LeakedFirecrackerPIDs  int
	StateRootFreeHeadroomG float64
	HasStateRootReading    bool
	Warnings               []string
}

// LeakAuditor verifies OSEP-0034 §4.6 zero-OOM, storage headroom, and resource cleanliness gates.
type LeakAuditor struct {
	cfg     Config
	clients *Clients
	scraper *PrometheusScraper
	obd     *OverlayBDProbe
}

// NewLeakAuditor constructs a LeakAuditor.
func NewLeakAuditor(cfg Config, clients *Clients) *LeakAuditor {
	cfg.applyDefaults()
	return &LeakAuditor{
		cfg:     cfg,
		clients: clients,
		scraper: NewPrometheusScraper(cfg, clients),
		obd:     NewOverlayBDProbe(cfg, clients),
	}
}

// CaptureBaseline records pre-run per-node /proc/vmstat oom_kill counters and
// fastlet podcgroup oom_kill totals so Audit() measures only OOM kills occurring
// during the benchmark run rather than cumulative counts since node boot.
func (a *LeakAuditor) CaptureBaseline(ctx context.Context) (map[string]int64, int64) {
	hostOOM := make(map[string]int64)
	var cgroupOOM int64
	if a == nil || a.clients == nil || a.clients.Clientset == nil || a.clients.Execer == nil {
		return hostOOM, 0
	}
	rtPods, err := listFirecrackerRuntimePods(ctx, a.clients.Clientset, a.cfg.SystemNamespace, a.cfg.ResourceNamespace)
	if err == nil {
		for i := range rtPods {
			pod := &rtPods[i]
			nodeName := pod.Spec.NodeName
			if nodeName == "" {
				nodeName = pod.Name
			}
			container := pickRuntimeContainer(pod, DefaultFirecrackerAgentContainer, DefaultJanitorContainer, DefaultUblkdContainer)
			stdout, _, execErr := execCommand(ctx, a.clients.Execer, pod.Namespace, pod.Name, container,
				[]string{"/bin/sh", "-c", `echo "oom_kill=$(awk '$1=="oom_kill"{print $2}' /proc/vmstat 2>/dev/null || echo 0)"`},
				a.cfg.ExecTimeout)
			if execErr != nil {
				continue
			}
			if kv := parseKeyValues(stdout); kv != nil {
				if v, ok := kv["oom_kill"]; ok {
					hostOOM[nodeName] = v
				}
			}
		}
	}
	// Prefer fastlet Prometheus fast_sandbox_runtime_cgroup_oom_kills_total; fall back to podcgroup exec probe.
	promOOMFound := false
	if a.scraper != nil {
		if promSnap, _ := a.scraper.Snapshot(ctx); promSnap != nil {
			if cgOOM, ok := podCgroupOOMFromPromSnapshot(promSnap); ok {
				cgroupOOM = cgOOM
				promOOMFound = true
			}
		}
	}
	if !promOOMFound {
		sampler := NewLoadSampler(a.cfg, a.clients, DefaultSamplerInterval)
		if cg, _ := sampler.sampleFastletPodCgroups(ctx); cg != nil {
			cgroupOOM = cg.MemoryEvents.OOMKill
		}
	}
	return hostOOM, cgroupOOM
}

// Audit inspects CRs, fastlet deleting slots, ublk devices, Firecracker PIDs, host/cgroup OOM kills,
// and StateRoot headroom, returning a pass/fail *report.LeakAudit.
func (a *LeakAuditor) Audit(ctx context.Context, opts LeakAuditOptions) (*report.LeakAudit, error) {
	var m LeakAuditMeasurements

	// 1. Residual Sandbox & SandboxSnapshot CRs via dynamic client.
	// Allow a brief settle window (up to 5s) when CRs are still finishing async K8s
	// finalizer teardown immediately after the last suite deletes its sandboxes.
	if a.clients != nil && a.clients.DynamicClient != nil {
		deadline := time.Now().Add(5 * time.Second)
		for {
			sbxCount, sbxErr := a.countCustomResources(ctx, opts.CRLabelSelector, SandboxGVRV1Alpha2, SandboxGVRV1Alpha1)
			snapCount, snapErr := a.countCustomResources(ctx, opts.CRLabelSelector, SandboxSnapshotGVRV1Alpha2, SandboxSnapshotGVRV1Alpha1)
			if sbxErr != nil {
				m.Warnings = append(m.Warnings, fmt.Sprintf("count Sandbox CRs: %v", sbxErr))
			} else {
				m.ResidualSandboxCRs = sbxCount
			}
			if snapErr != nil {
				m.Warnings = append(m.Warnings, fmt.Sprintf("count SandboxSnapshot CRs: %v", snapErr))
			} else {
				m.ResidualSnapshotCRs = snapCount
			}
			if (sbxErr != nil || sbxCount == 0) && (snapErr != nil || snapCount == 0) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				goto doneCRSettle
			case <-time.After(500 * time.Millisecond):
			}
		}
	doneCRSettle:
	}

	// 2. Stuck deleting slots and podcgroup OOM kills on fastlets via Prometheus scrape.
	var promSnap *PromSnapshot
	if a.scraper != nil && a.clients != nil && a.clients.Clientset != nil {
		promSnap, _ = a.scraper.Snapshot(ctx)
		if promSnap != nil && promSnap.Merged != nil {
			m.StuckDeletingSlots = int(math.Round(promSnap.Merged.Sum("fast_sandbox_fastlet_admission_slots", map[string]string{"state": "deleting"})))
		}
	}

	// 3. Runtime-node probes: orphaned ublk devices, leaked Firecracker PIDs, host OOM kills, StateRoot headroom.
	if a.clients != nil && a.clients.Clientset != nil && a.clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, a.clients.Clientset, a.cfg.SystemNamespace, a.cfg.ResourceNamespace)
		if err != nil {
			m.Warnings = append(m.Warnings, fmt.Sprintf("list firecracker-runtime pods: %v", err))
		} else {
			minFreeGiB := math.Inf(1)
			for i := range rtPods {
				pod := &rtPods[i]
				nodeName := pod.Spec.NodeName
				if nodeName == "" {
					nodeName = pod.Name
				}

				// Query warm pool idle count so warm pool devices are not miscounted as orphaned ublk devices.
				poolIdle := 0
				if poolSt, poolErr := a.obd.QueryPoolStatus(ctx, pod); poolErr == nil && poolSt != nil {
					poolIdle = poolSt.Idle
				}

				container := pickRuntimeContainer(pod, DefaultFirecrackerAgentContainer, DefaultJanitorContainer, DefaultUblkdContainer)
				script := fmt.Sprintf(`
echo "ublk_count=$(ls -1 /dev/ublkb* 2>/dev/null | wc -l | tr -d ' ')"
echo "fc_pids=$((pgrep -xc firecracker 2>/dev/null || pidof firecracker 2>/dev/null | wc -w || echo 0) | tr -d ' ')"
echo "oom_kill=$(awk '$1=="oom_kill"{print $2}' /proc/vmstat 2>/dev/null || echo 0)"
echo "stateroot_free_bytes=$(df -B1 %q 2>/dev/null | awk 'NR==2 {print $4}' || echo 0)"
`, a.cfg.StateRoot)
				stdout, stderr, execErr := execCommand(ctx, a.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", script}, a.cfg.ExecTimeout)
				if execErr != nil {
					m.Warnings = append(m.Warnings, fmt.Sprintf("node %s leak probe: %v (%s)", nodeName, execErr, strings.TrimSpace(stderr)))
					continue
				}
				kv := parseKeyValues(stdout)
				if ublkCount, ok := kv["ublk_count"]; ok {
					orphaned := int(ublkCount) - poolIdle
					if orphaned < 0 {
						orphaned = 0
					}
					m.OrphanedUblkDevices += orphaned
				}
				if fcPIDs, ok := kv["fc_pids"]; ok {
					m.LeakedFirecrackerPIDs += int(fcPIDs)
				}
				if oomTotal, ok := kv["oom_kill"]; ok {
					base := opts.BaselineHostOOMKills[nodeName]
					delta := oomTotal - base
					if delta > 0 {
						m.HostOOMKills += int(delta)
					}
				}
				if freeBytes, ok := kv["stateroot_free_bytes"]; ok && freeBytes > 0 {
					freeGiB := float64(freeBytes) / float64(bytesPerGiB)
					if freeGiB < minFreeGiB {
						minFreeGiB = freeGiB
						m.HasStateRootReading = true
					}
				}
			}
			if m.HasStateRootReading && !math.IsInf(minFreeGiB, 1) {
				m.StateRootFreeHeadroomG = minFreeGiB
			}
		}

		// 4. Fastlet sandbox-parent cgroup oom_kill check (Prometheus first, exec fallback).
		if cgOOM, ok := podCgroupOOMFromPromSnapshot(promSnap); ok {
			delta := cgOOM - opts.BaselineCgroupOOMKills
			if delta > 0 {
				m.PodCgroupOOMKills = int(delta)
			}
		} else {
			sampler := NewLoadSampler(a.cfg, a.clients, DefaultSamplerInterval)
			if cg, warn := sampler.sampleFastletPodCgroups(ctx); cg != nil {
				delta := cg.MemoryEvents.OOMKill - opts.BaselineCgroupOOMKills
				if delta > 0 {
					m.PodCgroupOOMKills = int(delta)
				}
			} else if warn != "" {
				m.Warnings = append(m.Warnings, warn)
			}
		}
	}

	return EvaluateLeakAudit(m, opts.MinStateRootFreeGiB), nil
}

func podCgroupOOMFromPromSnapshot(snap *PromSnapshot) (int64, bool) {
	if snap == nil {
		return 0, false
	}
	ms := snap.ByComponent[ComponentFSBFastlet]
	if ms == nil {
		ms = snap.Merged
	}
	if ms == nil {
		return 0, false
	}
	poolFilter := map[string]string{"sandbox_uid": ""}
	if !hasMatchingSample(ms, "fast_sandbox_runtime_cgroup_oom_kills_total", poolFilter) {
		return 0, false
	}
	return int64(math.Round(ms.Sum("fast_sandbox_runtime_cgroup_oom_kills_total", poolFilter))), true
}

// EvaluateLeakAudit applies OSEP-0034 §4.6 zero-OOM, headroom, and leak-free pass/fail gates
// to raw measurements, returning a populated *report.LeakAudit.
func EvaluateLeakAudit(m LeakAuditMeasurements, minStateRootFreeGiB float64) *report.LeakAudit {
	audit := &report.LeakAudit{
		Passed:                 true,
		HostOOMKills:           m.HostOOMKills,
		PodCgroupOOMKills:      m.PodCgroupOOMKills,
		ResidualSandboxCRs:     m.ResidualSandboxCRs,
		ResidualSnapshotCRs:    m.ResidualSnapshotCRs,
		StuckDeletingSlots:     m.StuckDeletingSlots,
		OrphanedUblkDevices:    m.OrphanedUblkDevices,
		LeakedFirecrackerPIDs:  m.LeakedFirecrackerPIDs,
		StateRootFreeHeadroomG: m.StateRootFreeHeadroomG,
	}

	fail := func(msg string) {
		audit.Passed = false
		audit.Failures = append(audit.Failures, msg)
	}

	if m.HostOOMKills > 0 {
		fail(fmt.Sprintf("host OOM kills detected: %d (expected 0)", m.HostOOMKills))
	}
	if m.PodCgroupOOMKills > 0 {
		fail(fmt.Sprintf("fastlet podcgroup OOM kills detected: %d (expected 0)", m.PodCgroupOOMKills))
	}
	if m.ResidualSandboxCRs > 0 {
		fail(fmt.Sprintf("residual Sandbox CRs found: %d (expected 0)", m.ResidualSandboxCRs))
	}
	if m.ResidualSnapshotCRs > 0 {
		fail(fmt.Sprintf("residual SandboxSnapshot CRs found: %d (expected 0)", m.ResidualSnapshotCRs))
	}
	if m.StuckDeletingSlots > 0 {
		fail(fmt.Sprintf("stuck deleting slots on fastlets: %d (expected 0)", m.StuckDeletingSlots))
	}
	if m.OrphanedUblkDevices > 0 {
		fail(fmt.Sprintf("orphaned ublk devices found: %d (expected 0)", m.OrphanedUblkDevices))
	}
	if m.LeakedFirecrackerPIDs > 0 {
		fail(fmt.Sprintf("leaked Firecracker PIDs found: %d (expected 0)", m.LeakedFirecrackerPIDs))
	}
	if minStateRootFreeGiB > 0 && m.HasStateRootReading && m.StateRootFreeHeadroomG < minStateRootFreeGiB {
		fail(fmt.Sprintf("StateRoot free headroom %.2f GiB is below configured floor %.2f GiB", m.StateRootFreeHeadroomG, minStateRootFreeGiB))
	}
	return audit
}

func (a *LeakAuditor) countCustomResources(ctx context.Context, labelSelector string, gvrs ...schema.GroupVersionResource) (int, error) {
	var lastErr error
	for _, gvr := range gvrs {
		list, err := a.clients.DynamicClient.Resource(gvr).Namespace(a.cfg.ResourceNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				lastErr = err
				continue
			}
			return 0, err
		}
		return len(list.Items), nil
	}
	if apierrors.IsNotFound(lastErr) {
		return 0, nil
	}
	return 0, lastErr
}

func parseKeyValues(raw string) map[string]int64 {
	out := make(map[string]int64)
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[strings.TrimSpace(k)] = n
		}
	}
	return out
}
