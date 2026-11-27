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

const (
	// DefaultLeakAuditGracePeriod is the default window to wait for async K8s finalizer
	// CR cleanup after a high-concurrency benchmark run before failing the residual CR check.
	DefaultLeakAuditGracePeriod = 30 * time.Second
	// DefaultLeakAuditPollInterval is the default polling cadence during CR convergence.
	DefaultLeakAuditPollInterval = 2 * time.Second
)

// LeakAuditOptions controls thresholds and baseline OOM/slot counts for post-run leak auditing.
type LeakAuditOptions struct {
	MinStateRootFreeGiB    float64
	BaselineHostOOMKills   map[string]int64 // optional per-node /proc/vmstat oom_kill baseline
	BaselineCgroupOOMKills int64            // optional fastlet podcgroup oom_kill baseline
	BaselineCleanSlots     int              // optional pre-run fastlet admission clean slots baseline
	CRLabelSelector        string           // optional label selector scoping audited CRs
	GracePeriod            time.Duration    // CR convergence grace window; 0 checks immediately
	PollInterval           time.Duration    // CR convergence poll interval; defaults to DefaultLeakAuditPollInterval
}

// LeakAuditBaseline captures pre-run OOM and fastlet admission slot baselines.
type LeakAuditBaseline struct {
	HostOOMKills          map[string]int64
	CgroupOOMKills        int64
	AdmissionSlotsByState map[string]int
	CleanSlots            int
	TotalSlots            int
}

// LeakAuditMeasurements holds raw observed counts before pass/fail gate evaluation.
type LeakAuditMeasurements struct {
	HostOOMKills           int
	PodCgroupOOMKills      int
	ResidualSandboxCRs     int
	ResidualSnapshotCRs    int
	StuckDeletingSlots     int
	AdmissionSlotsByState  map[string]int
	BaselineCleanSlots     int
	CleanSlotsAfter        int
	LostCleanSlots         int
	NonCleanResidualSlots  int
	OrphanedUblkDevices    int
	LeakedFirecrackerPIDs  int
	StateRootFreeHeadroomG float64
	HasStateRootReading    bool
	CRConvergenceSeconds   float64
	Warnings               []string
	ProbeErrors            []string
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
	base := a.CaptureFullBaseline(ctx)
	return base.HostOOMKills, base.CgroupOOMKills
}

// CaptureFullBaseline records pre-run host/cgroup OOM counters and fastlet
// admission slot counts by state (including baseline clean slots).
func (a *LeakAuditor) CaptureFullBaseline(ctx context.Context) LeakAuditBaseline {
	base := LeakAuditBaseline{
		HostOOMKills: make(map[string]int64),
	}
	if a == nil || a.clients == nil || a.clients.Clientset == nil {
		return base
	}
	if a.clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, a.clients.Clientset, a.cfg.SystemNamespace, a.cfg.ResourceNamespace)
		if err == nil {
			for i := range rtPods {
				pod := &rtPods[i]
				nodeName := pod.Spec.NodeName
				if nodeName == "" {
					nodeName = pod.Name
				}
				container := pickRuntimeContainer(pod, DefaultJanitorContainer, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
				stdout, _, execErr := execCommand(ctx, a.clients.Execer, pod.Namespace, pod.Name, container,
					[]string{"/bin/sh", "-c", `echo "oom_kill=$(awk '$1=="oom_kill"{print $2}' /proc/vmstat 2>/dev/null || echo 0)"`},
					a.cfg.ExecTimeout)
				if execErr != nil {
					continue
				}
				if kv := parseKeyValues(stdout); kv != nil {
					if v, ok := kv["oom_kill"]; ok {
						base.HostOOMKills[nodeName] = v
					}
				}
			}
		}
	}
	// Prefer fastlet Prometheus fast_sandbox_runtime_cgroup_oom_kills_total; also capture baseline admission slots.
	promOOMFound := false
	if a.scraper != nil {
		if promSnap, _ := a.scraper.Snapshot(ctx); promSnap != nil {
			if cgOOM, ok := podCgroupOOMFromPromSnapshot(promSnap); ok {
				base.CgroupOOMKills = cgOOM
				promOOMFound = true
			}
			if slots := extractAdmissionSlotsByState(promSnap); len(slots) > 0 {
				base.AdmissionSlotsByState = slots
				base.CleanSlots = slots["clean"]
				for _, v := range slots {
					base.TotalSlots += v
				}
			}
		}
	}
	if !promOOMFound && a.clients.Execer != nil {
		sampler := NewLoadSampler(a.cfg, a.clients, DefaultSamplerInterval)
		if cg, _ := sampler.sampleFastletPodCgroups(ctx); cg != nil {
			base.CgroupOOMKills = cg.MemoryEvents.OOMKill
		}
	}
	return base
}

func extractAdmissionSlotsByState(snap *PromSnapshot) map[string]int {
	if snap == nil {
		return nil
	}
	ms := snap.ByComponent[ComponentFSBFastlet]
	if ms == nil {
		ms = snap.Merged
	}
	if ms == nil {
		return nil
	}
	raw := ms.SumByLabel("fast_sandbox_fastlet_admission_slots", "state")
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]int, len(raw))
	for state, val := range raw {
		if state == "" {
			continue
		}
		out[state] = int(math.Round(val))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Audit inspects CRs, fastlet deleting slots, ublk devices, Firecracker PIDs, host/cgroup OOM kills,
// and StateRoot headroom, returning a pass/fail *report.LeakAudit.
func (a *LeakAuditor) Audit(ctx context.Context, opts LeakAuditOptions) (*report.LeakAudit, error) {
	var m LeakAuditMeasurements
	m.BaselineCleanSlots = opts.BaselineCleanSlots

	// 1. Residual Sandbox & SandboxSnapshot CRs via dynamic client.
	// Poll for CR convergence over opts.GracePeriod when residual CRs are finishing
	// async K8s finalizer teardown immediately after a high-concurrency run.
	if a.clients != nil && a.clients.DynamicClient != nil {
		pollInterval := opts.PollInterval
		if pollInterval <= 0 {
			pollInterval = DefaultLeakAuditPollInterval
		}
		settleStart := time.Now()
		deadline := settleStart.Add(opts.GracePeriod)
		polled := false
		for {
			sbxCount, sbxErr := a.countCustomResources(ctx, opts.CRLabelSelector, SandboxGVRV1Alpha2, SandboxGVRV1Alpha1)
			snapCount, snapErr := a.countCustomResources(ctx, opts.CRLabelSelector, SandboxSnapshotGVRV1Alpha2, SandboxSnapshotGVRV1Alpha1)
			if sbxErr != nil {
				msg := fmt.Sprintf("count Sandbox CRs: %v", sbxErr)
				m.Warnings = append(m.Warnings, msg)
				m.ProbeErrors = append(m.ProbeErrors, msg)
			} else {
				m.ResidualSandboxCRs = sbxCount
			}
			if snapErr != nil {
				msg := fmt.Sprintf("count SandboxSnapshot CRs: %v", snapErr)
				m.Warnings = append(m.Warnings, msg)
				m.ProbeErrors = append(m.ProbeErrors, msg)
			} else {
				m.ResidualSnapshotCRs = snapCount
			}
			if (sbxErr != nil || sbxCount == 0) && (snapErr != nil || snapCount == 0) {
				if polled {
					m.CRConvergenceSeconds = time.Since(settleStart).Seconds()
				}
				break
			}
			if opts.GracePeriod <= 0 || !time.Now().Before(deadline) {
				if polled {
					m.CRConvergenceSeconds = time.Since(settleStart).Seconds()
				}
				break
			}
			polled = true
			wait := pollInterval
			if rem := time.Until(deadline); rem < wait {
				wait = rem
			}
			if wait <= 0 {
				m.CRConvergenceSeconds = time.Since(settleStart).Seconds()
				break
			}
			select {
			case <-ctx.Done():
				m.CRConvergenceSeconds = time.Since(settleStart).Seconds()
				goto doneCRSettle
			case <-time.After(wait):
			}
		}
	doneCRSettle:
	}

	// 2. Fastlet admission slots (deleting, clean conservation, non-clean residuals) and podcgroup OOM kills via Prometheus scrape.
	var promSnap *PromSnapshot
	if a.scraper != nil && a.clients != nil && a.clients.Clientset != nil {
		promSnap, _ = a.scraper.Snapshot(ctx)
		if promSnap != nil {
			for _, w := range promSnap.Warnings {
				if strings.HasPrefix(w, "list pods in "+a.cfg.ResourceNamespace+":") ||
					strings.HasPrefix(w, "list pods in "+a.cfg.SystemNamespace+":") ||
					strings.HasPrefix(w, "scrape "+ComponentFSBFastlet+" ") {
					m.Warnings = append(m.Warnings, w)
					m.ProbeErrors = append(m.ProbeErrors, w)
				}
			}
			if slots := extractAdmissionSlotsByState(promSnap); len(slots) > 0 {
				m.AdmissionSlotsByState = slots
				m.StuckDeletingSlots = slots["deleting"]
				m.CleanSlotsAfter = slots["clean"]
				for state, count := range slots {
					if state != "clean" && count > 0 {
						m.NonCleanResidualSlots += count
					}
				}
				if opts.BaselineCleanSlots > 0 {
					if lost := opts.BaselineCleanSlots - m.CleanSlotsAfter; lost > 0 {
						m.LostCleanSlots = lost
					}
				}
			} else if opts.BaselineCleanSlots > 0 {
				msg := fmt.Sprintf("fastlet admission slot metrics unavailable after run (baseline clean=%d)", opts.BaselineCleanSlots)
				m.Warnings = append(m.Warnings, msg)
				m.ProbeErrors = append(m.ProbeErrors, msg)
			}
		}
	}

	// 3. Runtime-node probes: orphaned ublk devices, leaked Firecracker PIDs, host OOM kills, StateRoot headroom.
	if a.clients != nil && a.clients.Clientset != nil && a.clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, a.clients.Clientset, a.cfg.SystemNamespace, a.cfg.ResourceNamespace)
		if err != nil {
			msg := fmt.Sprintf("list firecracker-runtime pods: %v", err)
			m.Warnings = append(m.Warnings, msg)
			m.ProbeErrors = append(m.ProbeErrors, msg)
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

				container := pickRuntimeContainer(pod, DefaultJanitorContainer, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
				script := fmt.Sprintf(`
echo "ublk_count=$(ls -1 /dev/ublkb* 2>/dev/null | wc -l | tr -d ' ')"
fc_cnt=$(pgrep -xc firecracker 2>/dev/null || pidof firecracker 2>/dev/null | wc -w || echo 0)
echo "fc_pids=$(printf '%%s' "$fc_cnt" | tr -d ' \t\r\n')"
echo "oom_kill=$(awk '$1=="oom_kill"{print $2}' /proc/vmstat 2>/dev/null || echo 0)"
echo "stateroot_free_bytes=$(df -B1 %q 2>/dev/null | awk 'NR==2 {print $4}' || echo 0)"
`, a.cfg.StateRoot)
				stdout, stderr, execErr := execCommand(ctx, a.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", script}, a.cfg.ExecTimeout)
				if execErr != nil {
					msg := fmt.Sprintf("node %s leak probe: %v (%s)", nodeName, execErr, strings.TrimSpace(stderr))
					m.Warnings = append(m.Warnings, msg)
					m.ProbeErrors = append(m.ProbeErrors, msg)
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

// EvaluateLeakAudit applies OSEP-0034 §4.6 zero-OOM, headroom, slot conservation, and leak-free pass/fail gates
// to raw measurements, returning a populated *report.LeakAudit.
func EvaluateLeakAudit(m LeakAuditMeasurements, minStateRootFreeGiB float64) *report.LeakAudit {
	audit := &report.LeakAudit{
		Passed:                 true,
		HostOOMKills:           m.HostOOMKills,
		PodCgroupOOMKills:      m.PodCgroupOOMKills,
		ResidualSandboxCRs:     m.ResidualSandboxCRs,
		ResidualSnapshotCRs:    m.ResidualSnapshotCRs,
		StuckDeletingSlots:     m.StuckDeletingSlots,
		AdmissionSlotsByState:  m.AdmissionSlotsByState,
		BaselineCleanSlots:     m.BaselineCleanSlots,
		CleanSlotsAfter:        m.CleanSlotsAfter,
		LostCleanSlots:         m.LostCleanSlots,
		NonCleanResidualSlots:  m.NonCleanResidualSlots,
		OrphanedUblkDevices:    m.OrphanedUblkDevices,
		LeakedFirecrackerPIDs:  m.LeakedFirecrackerPIDs,
		StateRootFreeHeadroomG: m.StateRootFreeHeadroomG,
		CRConvergenceSeconds:   m.CRConvergenceSeconds,
		Warnings:               append([]string(nil), m.Warnings...),
	}

	fail := func(msg string) {
		audit.Passed = false
		audit.Failures = append(audit.Failures, msg)
	}

	if len(m.ProbeErrors) > 0 {
		audit.Incomplete = true
		for _, pe := range m.ProbeErrors {
			fail(fmt.Sprintf("audit_probe_failed: %s", pe))
		}
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
	if m.LostCleanSlots > 0 {
		fail(fmt.Sprintf("lost clean admission slots on fastlets: %d (baseline=%d, after=%d)", m.LostCleanSlots, m.BaselineCleanSlots, m.CleanSlotsAfter))
	}
	if m.NonCleanResidualSlots > 0 {
		fail(fmt.Sprintf("non-clean residual admission slots on fastlets: %d (states=%v)", m.NonCleanResidualSlots, m.AdmissionSlotsByState))
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
