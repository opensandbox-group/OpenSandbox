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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	DefaultSamplerInterval  = 2 * time.Second
	bytesPerMiB             = 1024 * 1024
	maxConcurrentNodeProbes = 8
)

// PSIMetrics captures Linux /proc/pressure/{cpu,memory,io} some/full averages.
type PSIMetrics struct {
	SomeAvg10  float64 `json:"some_avg10"`
	SomeAvg60  float64 `json:"some_avg60"`
	SomeAvg300 float64 `json:"some_avg300"`
	FullAvg10  float64 `json:"full_avg10"`
	FullAvg60  float64 `json:"full_avg60"`
	FullAvg300 float64 `json:"full_avg300"`
}

// HostSample represents one point-in-time telemetry sample from a runtime host node.
type HostSample struct {
	Timestamp             time.Time
	NodeName              string
	CPUUtilPct            float64
	CPUUserPct            float64
	CPUSysPct             float64
	CPUIOWaitPct          float64
	CPUStealPct           float64
	Load1m                float64
	MemTotalMiB           float64
	MemAvailableMiB       float64
	PageCacheMiB          float64
	CachedMiB             float64
	BuffersMiB            float64
	SlabMiB               float64
	SReclaimableMiB       float64
	SUnreclaimMiB         float64
	CPUPSI                PSIMetrics
	MemoryPSI             PSIMetrics
	IOPSI                 PSIMetrics
	OOMKillTotal          int64
	StateRootFreeGiB      float64
	ConntrackCount        int64
	ConntrackMax          int64
	ConntrackUnavailable  bool
	OpenFDs               int64
	ActiveUblkDevices     int
	FirecrackerPIDCount   int
	cpuStat               *procStatSample // internal for delta CPU utilization
	cpuDeltaPending       bool            // true on tick 0 before a prior /proc/stat exists
}

// HostMemoryAndProcessSnapshot captures a synchronized host memory, Firecracker
// smaps_rollup PSS/RSS, daemon RSS, and host CPU/load snapshot across runtime nodes.
type HostMemoryAndProcessSnapshot struct {
	Timestamp            time.Time `json:"timestamp"`
	NodeCount            int       `json:"node_count"`
	MemTotalMiB          float64   `json:"mem_total_mib"`
	MemAvailableMiB      float64   `json:"mem_available_mib"`
	CachedMiB            float64   `json:"cached_mib"`
	BuffersMiB           float64   `json:"buffers_mib"`
	SlabMiB              float64   `json:"slab_mib"`
	SReclaimableMiB      float64   `json:"sreclaimable_mib"`
	SUnreclaimMiB        float64   `json:"sunreclaim_mib"`
	FCPIDCount           int       `json:"fc_pid_count"`
	FCPssTotalMiB        float64   `json:"fc_pss_total_mib"`
	FCPssAvgMiB          float64   `json:"fc_pss_avg_mib"`
	FCRssTotalMiB        float64   `json:"fc_rss_total_mib"`
	FCRssAvgMiB          float64   `json:"fc_rss_avg_mib"`
	FastletRSSMiB        float64   `json:"fastlet_rss_mib"`
	EgressRSSMiB         float64   `json:"egress_rss_mib"`
	UblkdRSSMiB          float64   `json:"ublkd_rss_mib"`
	ProxyRSSMiB          float64   `json:"proxy_rss_mib"`
	CPUUtilPct           float64   `json:"cpu_util_pct"`
	Load1m               float64   `json:"load_1m"`
	CPUPressureSomeAvg60 float64   `json:"cpu_psi_some_avg60"`
	MemPressureFullAvg60 float64   `json:"mem_psi_full_avg60"`
}

// CgroupMemoryEvents models cgroup v2 memory.events counters.
type CgroupMemoryEvents struct {
	High    int64 `json:"high"`
	Max     int64 `json:"max"`
	OOM     int64 `json:"oom"`
	OOMKill int64 `json:"oom_kill"`
}

// PodCgroupSample models fastlet sandbox-parent cgroup accounting (internal/fastlet/podcgroup).
type PodCgroupSample struct {
	MemoryCurrentMiB float64
	MemoryMaxMiB     float64
	MemoryEvents     CgroupMemoryEvents
	MemoryPSI        PSIMetrics
	CPUPSI           PSIMetrics
}

// ComponentSample captures one point-in-time CPU/RSS/throttling and extra metrics for a stack component.
type ComponentSample struct {
	Timestamp        time.Time
	CPUCores         float64
	RSSMiB           float64
	NrThrottled      int64
	ThrottledUsec    int64
	Goroutines       float64
	OpenFDs          float64
	PodCgroup        *PodCgroupSample
	ExtraMetrics     map[string]float64
	cpuDeltaPending  bool // true on tick 0 before a prior process_cpu_seconds_total exists
}

// LoadSample bundles one synchronized sampling tick across host and stack components.
type LoadSample struct {
	Timestamp       time.Time
	ActiveSandboxes int
	Hosts           []HostSample
	Components      map[string]ComponentSample
	Warnings        []string
}

type procStatSample struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (p procStatSample) total() uint64 {
	return p.user + p.nice + p.system + p.idle + p.iowait + p.irq + p.softirq + p.steal
}

// LoadSampler runs a background goroutine collecting host and component load samples
// at a configurable interval (default 2s) and aggregating them into *report.LoadProfiles.
type LoadSampler struct {
	cfg        Config
	clients    *Clients
	scraper    *PrometheusScraper
	interval   time.Duration
	sampleFn   func(ctx context.Context) LoadSample // optional override for testing

	activeSandboxes atomic.Int64

	mu            sync.Mutex
	running       bool
	cancel        context.CancelFunc
	done          chan struct{}
	samples       []LoadSample
	warnings      []string
	prevCPUStat   map[string]procStatSample
	prevProcCPU   map[string]procCPUReading
	baselineOOM   map[string]int64
}

type procCPUReading struct {
	ts         time.Time
	cpuSeconds float64
}

// NewLoadSampler creates a LoadSampler with the given interval (defaults to 2s if <= 0).
func NewLoadSampler(cfg Config, clients *Clients, interval time.Duration) *LoadSampler {
	cfg.applyDefaults()
	if interval <= 0 {
		interval = DefaultSamplerInterval
	}
	return &LoadSampler{
		cfg:         cfg,
		clients:     clients,
		scraper:     NewPrometheusScraper(cfg, clients),
		interval:    interval,
		prevCPUStat: make(map[string]procStatSample),
		prevProcCPU: make(map[string]procCPUReading),
		baselineOOM: make(map[string]int64),
	}
}

// SetActiveSandboxes updates the active sandbox count reported on subsequent samples
// so per-sandbox resident memory and CPU overhead can be computed.
func (s *LoadSampler) SetActiveSandboxes(n int) {
	s.activeSandboxes.Store(int64(n))
}

// Start launches the background sampling goroutine. Calling Start on an already-running sampler is a no-op.
func (s *LoadSampler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.running = true
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.loop(runCtx)
}

// Stop halts the background sampling goroutine, waits for it to exit, and returns the aggregated LoadProfiles.
func (s *LoadSampler) Stop() *report.LoadProfiles {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return s.Aggregate()
	}
	cancel := s.cancel
	done := s.done
	s.running = false
	s.mu.Unlock()

	cancel()
	<-done
	return s.Aggregate()
}

// RecordSample appends an externally constructed LoadSample (useful for tests and custom probes).
func (s *LoadSampler) RecordSample(sample LoadSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sample.Timestamp.IsZero() {
		sample.Timestamp = time.Now().UTC()
	}
	for _, h := range sample.Hosts {
		if _, exists := s.baselineOOM[h.NodeName]; !exists {
			s.baselineOOM[h.NodeName] = h.OOMKillTotal
		}
	}
	s.samples = append(s.samples, sample)
	if len(sample.Warnings) > 0 {
		s.warnings = append(s.warnings, sample.Warnings...)
	}
}

// LatestHostSnapshot returns the most recent conservatively aggregated host metrics
// across all nodes (useful for per-tier density frontier rows), with OOMKillTotal
// summed as per-node deltas since each node's initial baseline reading.
func (s *LoadSampler) LatestHostSnapshot() (HostSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.samples) - 1; i >= 0; i-- {
		hosts := s.samples[i].Hosts
		if len(hosts) == 0 {
			continue
		}
		agg := hosts[0]
		agg.OOMKillTotal = s.oomDeltaLocked(agg)
		for _, h := range hosts[1:] {
			if h.Timestamp.After(agg.Timestamp) {
				agg.Timestamp = h.Timestamp
			}
			if h.CPUUtilPct > agg.CPUUtilPct {
				agg.CPUUtilPct = h.CPUUtilPct
			}
			if h.CPUUserPct > agg.CPUUserPct {
				agg.CPUUserPct = h.CPUUserPct
			}
			if h.CPUSysPct > agg.CPUSysPct {
				agg.CPUSysPct = h.CPUSysPct
			}
			if h.CPUIOWaitPct > agg.CPUIOWaitPct {
				agg.CPUIOWaitPct = h.CPUIOWaitPct
			}
			if h.CPUStealPct > agg.CPUStealPct {
				agg.CPUStealPct = h.CPUStealPct
			}
			if h.Load1m > agg.Load1m {
				agg.Load1m = h.Load1m
			}
			if h.MemAvailableMiB > 0 && (agg.MemAvailableMiB <= 0 || h.MemAvailableMiB < agg.MemAvailableMiB) {
				agg.MemAvailableMiB = h.MemAvailableMiB
			}
			if h.PageCacheMiB > 0 && (agg.PageCacheMiB <= 0 || h.PageCacheMiB < agg.PageCacheMiB) {
				agg.PageCacheMiB = h.PageCacheMiB
			}
			agg.CPUPSI = maxPSI(agg.CPUPSI, h.CPUPSI)
			agg.MemoryPSI = maxPSI(agg.MemoryPSI, h.MemoryPSI)
			agg.IOPSI = maxPSI(agg.IOPSI, h.IOPSI)
			if h.StateRootFreeGiB > 0 && (agg.StateRootFreeGiB <= 0 || h.StateRootFreeGiB < agg.StateRootFreeGiB) {
				agg.StateRootFreeGiB = h.StateRootFreeGiB
			}
			if h.ConntrackCount > agg.ConntrackCount {
				agg.ConntrackCount = h.ConntrackCount
			}
			if h.ConntrackMax > agg.ConntrackMax {
				agg.ConntrackMax = h.ConntrackMax
			}
			if h.OpenFDs > agg.OpenFDs {
				agg.OpenFDs = h.OpenFDs
			}
			agg.OOMKillTotal += s.oomDeltaLocked(h)
			agg.ActiveUblkDevices += h.ActiveUblkDevices
			agg.FirecrackerPIDCount += h.FirecrackerPIDCount
		}
		return agg, true
	}
	return HostSample{}, false
}

func (s *LoadSampler) oomDeltaLocked(h HostSample) int64 {
	if base, ok := s.baselineOOM[h.NodeName]; ok {
		delta := h.OOMKillTotal - base
		if delta < 0 {
			return 0
		}
		return delta
	}
	return h.OOMKillTotal
}

// LatestPodCgroupOOMDelta returns the delta in fastlet sandbox-parent cgroup OOM kills
// relative to the first recorded fastlet podcgroup sample.
func (s *LoadSampler) LatestPodCgroupOOMDelta() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		hasBase bool
		base    int64
		maxD    int64
	)
	for _, sm := range s.samples {
		cs, ok := sm.Components[ComponentFSBFastlet]
		if !ok || cs.PodCgroup == nil {
			continue
		}
		cur := cs.PodCgroup.MemoryEvents.OOMKill
		if !hasBase {
			base = cur
			hasBase = true
			continue
		}
		if d := cur - base; d > maxD {
			maxD = d
		}
	}
	return maxD
}

// LatestDeletingSlots returns the most recently scraped fastlet admission deleting slot count.
func (s *LoadSampler) LatestDeletingSlots() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.samples) - 1; i >= 0; i-- {
		cs, ok := s.samples[i].Components[ComponentFSBFastlet]
		if !ok || cs.ExtraMetrics == nil {
			continue
		}
		if v, exists := cs.ExtraMetrics["admission_slots_deleting"]; exists {
			return int(math.Round(v)), true
		}
	}
	return 0, false
}

// PeakActiveSandboxes returns the peak active sandbox count observed across all samples,
// considering both SetActiveSandboxes and total host Firecracker PID counts.
func (s *LoadSampler) PeakActiveSandboxes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	peak := 0
	for _, sm := range s.samples {
		if sm.ActiveSandboxes > peak {
			peak = sm.ActiveSandboxes
		}
		fcSum := 0
		for _, h := range sm.Hosts {
			fcSum += h.FirecrackerPIDCount
		}
		if fcSum > peak {
			peak = fcSum
		}
	}
	return peak
}

func maxPSI(a, b PSIMetrics) PSIMetrics {
	return PSIMetrics{
		SomeAvg10:  math.Max(a.SomeAvg10, b.SomeAvg10),
		SomeAvg60:  math.Max(a.SomeAvg60, b.SomeAvg60),
		SomeAvg300: math.Max(a.SomeAvg300, b.SomeAvg300),
		FullAvg10:  math.Max(a.FullAvg10, b.FullAvg10),
		FullAvg60:  math.Max(a.FullAvg60, b.FullAvg60),
		FullAvg300: math.Max(a.FullAvg300, b.FullAvg300),
	}
}

func (s *LoadSampler) loop(ctx context.Context) {
	defer close(s.done)
	// Take an immediate initial sample so short runs still capture telemetry.
	s.collectOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collectOnce(ctx)
		}
	}
}

func (s *LoadSampler) collectOnce(ctx context.Context) {
	if s.sampleFn != nil {
		s.RecordSample(s.sampleFn(ctx))
		return
	}
	sample := LoadSample{
		Timestamp:       time.Now().UTC(),
		ActiveSandboxes: int(s.activeSandboxes.Load()),
		Components:      make(map[string]ComponentSample),
	}

	// 1. Sample host nodes concurrently (bounded) via firecracker-runtime DaemonSet pods (hostPID + StateRoot mounted).
	if s.clients != nil && s.clients.Clientset != nil && s.clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, s.clients.Clientset, s.cfg.SystemNamespace, s.cfg.ResourceNamespace)
		if err != nil {
			sample.Warnings = append(sample.Warnings, fmt.Sprintf("list firecracker-runtime pods: %v", err))
		} else if len(rtPods) > 0 {
			type nodeProbeRes struct {
				hs   *HostSample
				warn string
			}
			results := make([]nodeProbeRes, len(rtPods))
			sem := make(chan struct{}, maxConcurrentNodeProbes)
			var wg sync.WaitGroup
			for i := range rtPods {
				wg.Add(1)
				go func(idx int, p *corev1.Pod) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					hs, warn := s.sampleHostNode(ctx, p)
					results[idx] = nodeProbeRes{hs: hs, warn: warn}
				}(i, &rtPods[i])
			}
			wg.Wait()
			for _, r := range results {
				if r.warn != "" {
					sample.Warnings = append(sample.Warnings, r.warn)
				}
				if r.hs != nil {
					sample.Hosts = append(sample.Hosts, *r.hs)
				}
			}
		}
	}

	// 2. Scrape Prometheus /metrics across all stack components for CPU, RSS, goroutines, FDs, and domain metrics.
	if s.scraper != nil && s.clients != nil && s.clients.Clientset != nil {
		promSnap, _ := s.scraper.Snapshot(ctx)
		if promSnap != nil {
			sample.Warnings = append(sample.Warnings, promSnap.Warnings...)
			s.populateComponentsFromProm(promSnap, &sample)
		}
	}

	// 3. Probe fastlet sandbox-parent cgroup (podcgroup) via pods/exec only when
	// Prometheus /metrics did not already supply fast_sandbox_runtime_cgroup_* telemetry.
	if sample.Components[ComponentFSBFastlet].PodCgroup == nil && s.clients != nil && s.clients.Clientset != nil && s.clients.Execer != nil {
		if cg, warn := s.sampleFastletPodCgroups(ctx); cg != nil {
			comp := sample.Components[ComponentFSBFastlet]
			comp.PodCgroup = cg
			sample.Components[ComponentFSBFastlet] = comp
		} else if warn != "" {
			sample.Warnings = append(sample.Warnings, warn)
		}
	}

	s.RecordSample(sample)
}

func (s *LoadSampler) sampleHostNode(ctx context.Context, pod *corev1.Pod) (*HostSample, string) {
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		nodeName = pod.Name
	}
	container := pickRuntimeContainer(pod, DefaultJanitorContainer, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
	script := fmt.Sprintf(`
echo "===STAT==="
head -n 1 /proc/stat 2>/dev/null || true
echo "===LOADAVG==="
cat /proc/loadavg 2>/dev/null || true
echo "===MEMINFO==="
grep -E '^(MemTotal|MemAvailable|Cached|Buffers|Slab|SReclaimable|SUnreclaim):' /proc/meminfo 2>/dev/null || true
echo "===PSI_CPU==="
cat /proc/pressure/cpu 2>/dev/null || true
echo "===PSI_MEM==="
cat /proc/pressure/memory 2>/dev/null || true
echo "===PSI_IO==="
cat /proc/pressure/io 2>/dev/null || true
echo "===VMSTAT==="
grep -E '^oom_kill ' /proc/vmstat 2>/dev/null || true
echo "===CONNTRACK==="
if command -v nsenter >/dev/null 2>&1; then
  nsenter -t 1 -n -- cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo unavailable
  nsenter -t 1 -n -- cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo unavailable
else
  echo unavailable
  echo unavailable
fi
echo "===FILENR==="
cat /proc/sys/fs/file-nr 2>/dev/null || true
echo "===STATEROOT_FREE==="
df -B1 %q 2>/dev/null | awk 'NR==2 {print $4}' || echo 0
echo "===UBLK_COUNT==="
ls -1 /dev/ublkb* 2>/dev/null | wc -l | tr -d ' '
echo "===FC_PIDS==="
(pgrep -xc firecracker 2>/dev/null || pidof firecracker 2>/dev/null | wc -w || echo 0) | tr -d ' '
`, s.cfg.StateRoot)

	stdout, stderr, err := execCommand(ctx, s.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", script}, s.cfg.ExecTimeout)
	if err != nil {
		return nil, fmt.Sprintf("host sample %s: %v (%s)", nodeName, err, strings.TrimSpace(stderr))
	}

	hs := ParseHostProbeOutput(nodeName, stdout)
	hs.Timestamp = time.Now().UTC()

	// Compute CPU utilization delta against previous /proc/stat sample for this node.
	if hs.cpuStat != nil {
		s.mu.Lock()
		prev, ok := s.prevCPUStat[nodeName]
		s.prevCPUStat[nodeName] = *hs.cpuStat
		s.mu.Unlock()
		if ok {
			computeCPUUtilDelta(prev, *hs.cpuStat, &hs)
		} else {
			hs.cpuDeltaPending = true
		}
	}
	return &hs, ""
}

func (s *LoadSampler) sampleFastletPodCgroups(ctx context.Context) (*PodCgroupSample, string) {
	podList, err := s.clients.Clientset.CoreV1().Pods(s.cfg.ResourceNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Sprintf("list fastlet pods for podcgroup: %v", err)
	}
	var fastletPods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || !isFastletPod(pod) {
			continue
		}
		fastletPods = append(fastletPods, pod)
	}
	if len(fastletPods) == 0 {
		return nil, ""
	}

	results := make([]*PodCgroupSample, len(fastletPods))
	sem := make(chan struct{}, maxConcurrentNodeProbes)
	var wg sync.WaitGroup
	for i, pod := range fastletPods {
		wg.Add(1)
		go func(idx int, p *corev1.Pod) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			uid := string(p.UID)
			uidUS := strings.ReplaceAll(uid, "-", "_")
			// Real sandbox-parent cgroup is /host/sys/fs/cgroup/<podPath>/fast-sandbox (podcgroup/layout.go).
			script := fmt.Sprintf(`
POD_UID=%q
POD_UID_US=%q
CG=""
if [ -d /host/sys/fs/cgroup ]; then
  if [ -n "$POD_UID" ]; then
    for d in $(find /host/sys/fs/cgroup -maxdepth 5 -type d \( -name "*${POD_UID}*" -o -name "*${POD_UID_US}*" \) 2>/dev/null); do
      if [ -f "$d/fast-sandbox/memory.current" ]; then
        CG="$d/fast-sandbox"
        break
      fi
    done
  fi
  if [ -z "$CG" ]; then
    for f in $(find /host/sys/fs/cgroup -maxdepth 6 -path "*/fast-sandbox/memory.current" 2>/dev/null | head -n 1); do
      CG=$(dirname "$f")
      break
    done
  fi
fi
if [ -z "$CG" ]; then
  for candidate in /sys/fs/cgroup/fast-sandbox /sys/fs/cgroup/sandboxes /sys/fs/cgroup; do
    if [ -f "$candidate/memory.current" ]; then
      CG="$candidate"
      break
    fi
  done
fi
echo "===MEM_CUR==="
cat "$CG/memory.current" 2>/dev/null || echo 0
echo "===MEM_MAX==="
cat "$CG/memory.max" 2>/dev/null || echo max
echo "===MEM_EVENTS==="
cat "$CG/memory.events" 2>/dev/null || true
echo "===MEM_PSI==="
cat "$CG/memory.pressure" 2>/dev/null || true
echo "===CPU_PSI==="
cat "$CG/cpu.pressure" 2>/dev/null || true
`, uid, uidUS)
			container := pickRuntimeContainer(p, "fastlet", "fsb-fastlet")
			stdout, _, execErr := execCommand(ctx, s.clients.Execer, p.Namespace, p.Name, container, []string{"/bin/sh", "-c", script}, s.cfg.ExecTimeout)
			if execErr != nil {
				return
			}
			cg := ParsePodCgroupOutput(stdout)
			results[idx] = &cg
		}(i, pod)
	}
	wg.Wait()

	var agg PodCgroupSample
	found := false
	for _, cg := range results {
		if cg == nil {
			continue
		}
		agg.MemoryCurrentMiB += cg.MemoryCurrentMiB
		agg.MemoryMaxMiB += cg.MemoryMaxMiB
		agg.MemoryEvents.High += cg.MemoryEvents.High
		agg.MemoryEvents.Max += cg.MemoryEvents.Max
		agg.MemoryEvents.OOM += cg.MemoryEvents.OOM
		agg.MemoryEvents.OOMKill += cg.MemoryEvents.OOMKill
		agg.MemoryPSI = maxPSI(agg.MemoryPSI, cg.MemoryPSI)
		agg.CPUPSI = maxPSI(agg.CPUPSI, cg.CPUPSI)
		found = true
	}
	if !found {
		return nil, ""
	}
	return &agg, ""
}

func (s *LoadSampler) populateComponentsFromProm(snap *PromSnapshot, sample *LoadSample) {
	now := snap.Timestamp
	// 1. Direct Prometheus /metrics components (fast-sandbox Go binaries).
	for _, comp := range []string{
		ComponentFSBFastlet,
		ComponentFSBFastletProxy,
		ComponentFirecrackerRuntime,
	} {
		ms := snap.ByComponent[comp]
		if ms == nil {
			continue
		}
		cs := ComponentSample{
			Timestamp:    now,
			RSSMiB:       ms.Sum("process_resident_memory_bytes", nil) / float64(bytesPerMiB),
			Goroutines:   ms.Sum("go_goroutines", nil),
			OpenFDs:      ms.Sum("process_open_fds", nil),
			ExtraMetrics: make(map[string]float64),
		}
		cpuSec := ms.Sum("process_cpu_seconds_total", nil)
		if cpuSec > 0 {
			s.computeComponentCPUDelta(comp, now, cpuSec, &cs)
		}
		switch comp {
		case ComponentFSBFastlet:
			for state, val := range ms.SumByLabel("fast_sandbox_fastlet_admission_slots", "state") {
				if state != "" {
					cs.ExtraMetrics["admission_slots_"+state] = val
				}
			}
			if cg, throttled, ok := podCgroupFromProm(ms); ok {
				cs.PodCgroup = cg
				if throttled > cs.NrThrottled {
					cs.NrThrottled = throttled
				}
			}
		case ComponentFSBFastletProxy:
			if v := ms.Sum("fast_sandbox_fastlet_proxy_upstream_latency_seconds_count", nil); v > 0 {
				cs.ExtraMetrics["upstream_requests_total"] = v
			}
		}
		sample.Components[comp] = cs
	}

	// 2. OSEP-0010 OTLP-only components (egress, ingress-gateway, opensandbox-server),
	// scraped from the in-cluster OTel Collector Prometheus exporter (:8889/metrics).
	otelMS := snap.ByComponent[ComponentOTelCollector]
	if otelMS == nil {
		otelMS = snap.Merged
	}
	if otelMS != nil {
		s.populateOTLPComponents(otelMS, now, sample)
	}
}

func (s *LoadSampler) computeComponentCPUDelta(comp string, now time.Time, cpuSec float64, cs *ComponentSample) {
	s.mu.Lock()
	prev, ok := s.prevProcCPU[comp]
	s.prevProcCPU[comp] = procCPUReading{ts: now, cpuSeconds: cpuSec}
	s.mu.Unlock()
	if !ok {
		cs.cpuDeltaPending = true
		return
	}
	dt := now.Sub(prev.ts).Seconds()
	if dt > 0 && cpuSec >= prev.cpuSeconds {
		cs.CPUCores = (cpuSec - prev.cpuSeconds) / dt
	}
}

func firstNonZeroMetricSum(ms *MetricSet, names ...string) (float64, bool) {
	for _, n := range names {
		if len(ms.Samples[n]) > 0 {
			return ms.Sum(n, nil), true
		}
	}
	return 0, false
}

func (s *LoadSampler) populateOTLPComponents(ms *MetricSet, now time.Time, sample *LoadSample) {
	// Egress OTLP metrics (components/egress/pkg/telemetry/metrics.go).
	egress := ComponentSample{Timestamp: now, ExtraMetrics: make(map[string]float64)}
	egressSeen := false
	if v, ok := firstNonZeroMetricSum(ms, "egress_process_memory_usage_bytes", "egress_process_memory_usage_bytes_bytes"); ok {
		egress.RSSMiB = v / float64(bytesPerMiB)
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_process_cpu_time_seconds_total", "egress_process_cpu_time_total"); ok && v > 0 {
		s.computeComponentCPUDelta(ComponentEgress, now, v, &egress)
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_dns_query_duration_seconds_count", "egress_dns_query_duration_count"); ok {
		egress.ExtraMetrics["dns_queries_total"] = v
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_dns_query_failed_total"); ok {
		egress.ExtraMetrics["dns_query_failed_total"] = v
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_policy_denied_total"); ok {
		egress.ExtraMetrics["policy_denied_total"] = v
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_nftables_rules_count", "egress_nftables_rules_count_element", "egress_nftables_rules_count_elements"); ok {
		egress.ExtraMetrics["nftables_rules"] = v
		egressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "egress_nftables_updates_count_total", "egress_nftables_updates_count"); ok {
		egress.ExtraMetrics["nftables_updates_total"] = v
		egressSeen = true
	}
	if egressSeen {
		sample.Components[ComponentEgress] = egress
	}

	// Ingress Gateway OTLP metrics (components/ingress/pkg/telemetry/metrics.go).
	ingress := ComponentSample{Timestamp: now, ExtraMetrics: make(map[string]float64)}
	ingressSeen := false
	if v, ok := firstNonZeroMetricSum(ms, "ingress_system_memory_usage_bytes", "ingress_system_memory_usage_bytes_bytes"); ok {
		ingress.RSSMiB = v / float64(bytesPerMiB)
		ingressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "ingress_park_active"); ok {
		ingress.ExtraMetrics["wake_parked_requests"] = v
		ingressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "ingress_wake_restore_active"); ok {
		ingress.ExtraMetrics["wake_restore_active"] = v
		ingressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "ingress_park_shed_total", "ingress_park_shed"); ok {
		ingress.ExtraMetrics["wake_park_shed_total"] = v
		ingressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "ingress_wake_flights_total", "ingress_wake_flights"); ok {
		ingress.ExtraMetrics["wake_flights_total"] = v
		ingressSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "ingress_connections_active"); ok {
		ingress.ExtraMetrics["active_connections"] = v
		ingressSeen = true
	}
	if ingressSeen {
		sample.Components[ComponentIngressGateway] = ingress
	}

	// OpenSandbox Server OTLP metrics (server/opensandbox_server/integrations/otel/metrics.py).
	srv := ComponentSample{Timestamp: now, ExtraMetrics: make(map[string]float64)}
	srvSeen := false
	if v, ok := firstNonZeroMetricSum(ms, "server_http_request_duration_milliseconds_count", "server_http_request_duration_count"); ok {
		srv.ExtraMetrics["http_requests_total"] = v
		srvSeen = true
	}
	if v, ok := firstNonZeroMetricSum(ms, "opensandbox_sandbox_create_duration_milliseconds_count", "opensandbox_sandbox_create_duration_count"); ok {
		srv.ExtraMetrics["sandbox_creates_total"] = v
		srvSeen = true
	}
	if srvSeen {
		sample.Components[ComponentOpenSandboxServer] = srv
	}
}

func podCgroupFromProm(ms *MetricSet) (*PodCgroupSample, int64, bool) {
	if ms == nil {
		return nil, 0, false
	}
	poolFilter := map[string]string{"sandbox_uid": ""}
	hasPoolMetric := func(name string) bool {
		for _, sm := range ms.Samples[name] {
			if matchesLabels(sm.Labels, poolFilter) {
				return true
			}
		}
		return false
	}

	metricNames := []string{
		"fast_sandbox_runtime_cgroup_memory_bytes",
		"fast_sandbox_runtime_cgroup_memory_limit_bytes",
		"fast_sandbox_runtime_cgroup_oom_kills_total",
		"fast_sandbox_runtime_cgroup_memory_psi_some_avg10",
		"fast_sandbox_runtime_cgroup_cpu_throttled_periods_total",
	}
	present := false
	for _, name := range metricNames {
		if hasPoolMetric(name) {
			present = true
			break
		}
	}
	if !present {
		return nil, 0, false
	}

	cg := &PodCgroupSample{
		MemoryCurrentMiB: ms.Sum("fast_sandbox_runtime_cgroup_memory_bytes", poolFilter) / float64(bytesPerMiB),
		MemoryMaxMiB:     ms.Sum("fast_sandbox_runtime_cgroup_memory_limit_bytes", poolFilter) / float64(bytesPerMiB),
		MemoryEvents: CgroupMemoryEvents{
			OOMKill: int64(math.Round(ms.Sum("fast_sandbox_runtime_cgroup_oom_kills_total", poolFilter))),
		},
	}
	if psi, ok := ms.Max("fast_sandbox_runtime_cgroup_memory_psi_some_avg10", poolFilter); ok {
		cg.MemoryPSI.SomeAvg10 = psi
	}
	throttled := int64(math.Round(ms.Sum("fast_sandbox_runtime_cgroup_cpu_throttled_periods_total", poolFilter)))
	return cg, throttled, true
}

// Aggregate summarizes all recorded LoadSamples into *report.LoadProfiles (Avg / P95 / Max).
func (s *LoadSampler) Aggregate() *report.LoadProfiles {
	s.mu.Lock()
	samples := append([]LoadSample(nil), s.samples...)
	warnings := append([]string(nil), s.warnings...)
	baselineOOM := make(map[string]int64, len(s.baselineOOM))
	for k, v := range s.baselineOOM {
		baselineOOM[k] = v
	}
	s.mu.Unlock()

	lp := &report.LoadProfiles{}
	if len(samples) == 0 {
		return lp
	}

	lp.Host = aggregateHostSamples(samples, baselineOOM, warnings)
	lp.Fastlet = aggregateComponentSamples(samples, ComponentFSBFastlet)
	lp.Egress = aggregateComponentSamples(samples, ComponentEgress)
	lp.FastletProxy = aggregateComponentSamples(samples, ComponentFSBFastletProxy)
	lp.FirecrackerRuntime = aggregateComponentSamples(samples, ComponentFirecrackerRuntime)
	lp.OpenSandboxServer = aggregateComponentSamples(samples, ComponentOpenSandboxServer)
	lp.IngressGateway = aggregateComponentSamples(samples, ComponentIngressGateway)

	return lp
}

func aggregateHostSamples(samples []LoadSample, baselineOOM map[string]int64, warnings []string) *report.ComponentLoadSummary {
	var (
		cpuUtil, cpuUser, cpuSys, cpuIOWait, cpuSteal []float64
		load1m, memAvail, pageCache                   []float64
		cpuPSISome10, cpuPSISome60, cpuPSISome300     []float64
		memPSISome10, memPSISome60, memPSIFull60      []float64
		ioPSISome60, ioPSIFull60                      []float64
		stateRootFree, conntrackRatio, openFDs        []float64
		ublkCounts, fcPIDCounts                       []float64
		conntrackUnavailable                          bool
		maxOOMDeltaByNode                             = make(map[string]int64)
	)
	for _, sm := range samples {
		for _, h := range sm.Hosts {
			if !h.cpuDeltaPending {
				cpuUtil = append(cpuUtil, h.CPUUtilPct)
				cpuUser = append(cpuUser, h.CPUUserPct)
				cpuSys = append(cpuSys, h.CPUSysPct)
				cpuIOWait = append(cpuIOWait, h.CPUIOWaitPct)
				cpuSteal = append(cpuSteal, h.CPUStealPct)
			}
			load1m = append(load1m, h.Load1m)
			if h.MemAvailableMiB > 0 {
				memAvail = append(memAvail, h.MemAvailableMiB)
			}
			if h.PageCacheMiB > 0 {
				pageCache = append(pageCache, h.PageCacheMiB)
			}
			cpuPSISome10 = append(cpuPSISome10, h.CPUPSI.SomeAvg10)
			cpuPSISome60 = append(cpuPSISome60, h.CPUPSI.SomeAvg60)
			cpuPSISome300 = append(cpuPSISome300, h.CPUPSI.SomeAvg300)
			memPSISome10 = append(memPSISome10, h.MemoryPSI.SomeAvg10)
			memPSISome60 = append(memPSISome60, h.MemoryPSI.SomeAvg60)
			memPSIFull60 = append(memPSIFull60, h.MemoryPSI.FullAvg60)
			ioPSISome60 = append(ioPSISome60, h.IOPSI.SomeAvg60)
			ioPSIFull60 = append(ioPSIFull60, h.IOPSI.FullAvg60)
			if h.StateRootFreeGiB > 0 {
				stateRootFree = append(stateRootFree, h.StateRootFreeGiB)
			}
			if h.ConntrackMax > 0 && !h.ConntrackUnavailable {
				conntrackRatio = append(conntrackRatio, float64(h.ConntrackCount)/float64(h.ConntrackMax))
			} else if h.ConntrackUnavailable {
				conntrackUnavailable = true
			}
			if h.OpenFDs > 0 {
				openFDs = append(openFDs, float64(h.OpenFDs))
			}
			ublkCounts = append(ublkCounts, float64(h.ActiveUblkDevices))
			fcPIDCounts = append(fcPIDCounts, float64(h.FirecrackerPIDCount))

			delta := h.OOMKillTotal - baselineOOM[h.NodeName]
			if delta < 0 {
				delta = 0
			}
			if delta > maxOOMDeltaByNode[h.NodeName] {
				maxOOMDeltaByNode[h.NodeName] = delta
			}
		}
	}
	if len(load1m) == 0 && len(cpuUtil) == 0 && len(warnings) == 0 {
		return nil
	}

	var totalOOMKills int64
	for _, d := range maxOOMDeltaByNode {
		totalOOMKills += d
	}

	metrics := map[string]any{
		"cpu_util_pct":          SummarizeStats(cpuUtil),
		"cpu_user_pct":          SummarizeStats(cpuUser),
		"cpu_sys_pct":           SummarizeStats(cpuSys),
		"cpu_iowait_pct":        SummarizeStats(cpuIOWait),
		"cpu_steal_pct":         SummarizeStats(cpuSteal),
		"load_1m":               SummarizeStats(load1m),
		"cpu_psi_some_avg10":    SummarizeStats(cpuPSISome10),
		"cpu_psi_some_avg60":    SummarizeStats(cpuPSISome60),
		"cpu_psi_some_avg300":   SummarizeStats(cpuPSISome300),
		"mem_psi_some_avg10":    SummarizeStats(memPSISome10),
		"mem_psi_some_avg60":    SummarizeStats(memPSISome60),
		"mem_psi_full_avg60":    SummarizeStats(memPSIFull60),
		"io_psi_some_avg60":     SummarizeStats(ioPSISome60),
		"io_psi_full_avg60":     SummarizeStats(ioPSIFull60),
		"host_oom_kills":        totalOOMKills,
		"active_ublk_devices":   SummarizeStats(ublkCounts),
		"firecracker_pid_count": SummarizeStats(fcPIDCounts),
	}
	if len(memAvail) > 0 {
		metrics["mem_available_mib"] = SummarizeStats(memAvail)
		metrics["mem_available_min_mib"] = minFloat(memAvail)
		metrics["mem_available_p5_mib"] = percentileNearestRank(memAvail, 5)
	}
	if len(pageCache) > 0 {
		metrics["page_cache_mib"] = SummarizeStats(pageCache)
	}
	if len(stateRootFree) > 0 {
		metrics["stateroot_free_headroom_gib_min"] = minFloat(stateRootFree)
		metrics["stateroot_free_headroom_gib"] = SummarizeStats(stateRootFree)
	}
	if len(conntrackRatio) > 0 {
		metrics["conntrack_usage_ratio"] = SummarizeStats(conntrackRatio)
	} else if conntrackUnavailable {
		metrics["conntrack_status"] = "unavailable"
	}
	if len(openFDs) > 0 {
		metrics["open_fds"] = SummarizeStats(openFDs)
	}
	if len(warnings) > 0 {
		metrics["warnings"] = warnings
	}

	return &report.ComponentLoadSummary{
		CPUCores: SummarizeStats(cpuUtil),
		Metrics:  metrics,
	}
}

func aggregateComponentSamples(samples []LoadSample, component string) *report.ComponentLoadSummary {
	var (
		cpus, rss, goroutines, fds                []float64
		perSbxRSS, perSbxCPUMilli                 []float64
		cgMemCur, cgMemMax                        []float64
		cgMemPSI10, cgMemPSI60, cgMemPSIFull60    []float64
		cgCPUPSI10, cgCPUPSI60                    []float64
		hasBaseThrottle                           bool
		baseThrottled, baseThrottledUsec          int64
		maxThrottledDelta, maxThrottledUsecDelta  int64
		hasBaseCG                                 bool
		baseCGHigh, baseCGMax                     int64
		baseCGOOM, baseCGOOMKill                  int64
		maxCGHighDelta, maxCGMaxDelta             int64
		maxCGOOMDelta, maxCGOOMKillDelta          int64
		hasPodCgroup                              bool
		extraSeries                               = make(map[string][]float64)
	)
	for _, sm := range samples {
		cs, ok := sm.Components[component]
		if !ok {
			continue
		}
		if !cs.cpuDeltaPending {
			cpus = append(cpus, cs.CPUCores)
		}
		rss = append(rss, cs.RSSMiB)
		if cs.Goroutines > 0 {
			goroutines = append(goroutines, cs.Goroutines)
		}
		if cs.OpenFDs > 0 {
			fds = append(fds, cs.OpenFDs)
		}
		if !hasBaseThrottle {
			baseThrottled = cs.NrThrottled
			baseThrottledUsec = cs.ThrottledUsec
			hasBaseThrottle = true
		} else {
			if d := cs.NrThrottled - baseThrottled; d > maxThrottledDelta {
				maxThrottledDelta = d
			}
			if d := cs.ThrottledUsec - baseThrottledUsec; d > maxThrottledUsecDelta {
				maxThrottledUsecDelta = d
			}
		}

		activeSbx := sm.ActiveSandboxes
		if activeSbx <= 0 {
			for _, h := range sm.Hosts {
				activeSbx += h.FirecrackerPIDCount
			}
		}
		if activeSbx > 0 {
			if cs.RSSMiB > 0 {
				perSbxRSS = append(perSbxRSS, cs.RSSMiB/float64(activeSbx))
			}
			if !cs.cpuDeltaPending && cs.CPUCores > 0 {
				perSbxCPUMilli = append(perSbxCPUMilli, (cs.CPUCores*1000.0)/float64(activeSbx))
			}
		}
		if cs.PodCgroup != nil {
			hasPodCgroup = true
			cgMemCur = append(cgMemCur, cs.PodCgroup.MemoryCurrentMiB)
			if cs.PodCgroup.MemoryMaxMiB > 0 {
				cgMemMax = append(cgMemMax, cs.PodCgroup.MemoryMaxMiB)
			}
			cgMemPSI10 = append(cgMemPSI10, cs.PodCgroup.MemoryPSI.SomeAvg10)
			cgMemPSI60 = append(cgMemPSI60, cs.PodCgroup.MemoryPSI.SomeAvg60)
			cgMemPSIFull60 = append(cgMemPSIFull60, cs.PodCgroup.MemoryPSI.FullAvg60)
			cgCPUPSI10 = append(cgCPUPSI10, cs.PodCgroup.CPUPSI.SomeAvg10)
			cgCPUPSI60 = append(cgCPUPSI60, cs.PodCgroup.CPUPSI.SomeAvg60)

			if !hasBaseCG {
				baseCGHigh = cs.PodCgroup.MemoryEvents.High
				baseCGMax = cs.PodCgroup.MemoryEvents.Max
				baseCGOOM = cs.PodCgroup.MemoryEvents.OOM
				baseCGOOMKill = cs.PodCgroup.MemoryEvents.OOMKill
				hasBaseCG = true
			} else {
				if d := cs.PodCgroup.MemoryEvents.High - baseCGHigh; d > maxCGHighDelta {
					maxCGHighDelta = d
				}
				if d := cs.PodCgroup.MemoryEvents.Max - baseCGMax; d > maxCGMaxDelta {
					maxCGMaxDelta = d
				}
				if d := cs.PodCgroup.MemoryEvents.OOM - baseCGOOM; d > maxCGOOMDelta {
					maxCGOOMDelta = d
				}
				if d := cs.PodCgroup.MemoryEvents.OOMKill - baseCGOOMKill; d > maxCGOOMKillDelta {
					maxCGOOMKillDelta = d
				}
			}
		}
		for k, v := range cs.ExtraMetrics {
			extraSeries[k] = append(extraSeries[k], v)
		}
	}
	if len(cpus) == 0 && len(rss) == 0 && !hasPodCgroup && len(extraSeries) == 0 {
		return nil
	}

	summary := &report.ComponentLoadSummary{
		CPUCores: SummarizeStats(cpus),
		RSSMiB:   SummarizeStats(rss),
		Metrics:  make(map[string]any),
	}
	if len(perSbxRSS) > 0 {
		summary.PerSandboxResidentMiB = meanFloat(perSbxRSS)
	}
	if len(perSbxCPUMilli) > 0 {
		summary.PerSandboxCPUMilli = meanFloat(perSbxCPUMilli)
	}
	if maxThrottledDelta > 0 {
		summary.Metrics["nr_throttled"] = maxThrottledDelta
	}
	if maxThrottledUsecDelta > 0 {
		summary.Metrics["throttled_usec"] = maxThrottledUsecDelta
	}
	if len(goroutines) > 0 {
		summary.Metrics["goroutines"] = SummarizeStats(goroutines)
	}
	if len(fds) > 0 {
		summary.Metrics["open_fds"] = SummarizeStats(fds)
	}
	if hasPodCgroup {
		cgMap := map[string]any{
			"memory_current_mib": SummarizeStats(cgMemCur),
			"memory_events": map[string]int64{
				"high":     maxCGHighDelta,
				"max":      maxCGMaxDelta,
				"oom":      maxCGOOMDelta,
				"oom_kill": maxCGOOMKillDelta,
			},
			"memory_psi_some_avg10": SummarizeStats(cgMemPSI10),
			"memory_psi_some_avg60": SummarizeStats(cgMemPSI60),
			"memory_psi_full_avg60": SummarizeStats(cgMemPSIFull60),
			"cpu_psi_some_avg10":    SummarizeStats(cgCPUPSI10),
			"cpu_psi_some_avg60":    SummarizeStats(cgCPUPSI60),
		}
		if len(cgMemMax) > 0 {
			cgMap["memory_max_mib"] = maxFloat(cgMemMax)
		}
		summary.Metrics["podcgroup"] = cgMap
	}
	for k, vals := range extraSeries {
		summary.Metrics[k] = SummarizeStats(vals)
	}
	if len(summary.Metrics) == 0 {
		summary.Metrics = nil
	}
	return summary
}

// SummarizeStats computes Avg, P95 (nearest-rank), and Max over values.
func SummarizeStats(values []float64) *report.StatSummary {
	if len(values) == 0 {
		return nil
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return &report.StatSummary{
		Avg: sum / float64(len(sorted)),
		P95: nearestRankSorted(sorted, 95),
		Max: sorted[len(sorted)-1],
	}
}

func percentileNearestRank(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	return nearestRankSorted(sorted, p)
}

func nearestRankSorted(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil((p / 100.0) * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func minFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := values[0]
	for _, v := range values[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func meanFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// ParseHostProbeOutput parses the sectioned shell output produced by sampleHostNode.
func ParseHostProbeOutput(nodeName, raw string) HostSample {
	hs := HostSample{NodeName: nodeName}
	sections := splitSections(raw)

	if st, ok := ParseProcStat(sections["STAT"]); ok {
		hs.cpuStat = &st
	}
	if fields := strings.Fields(sections["LOADAVG"]); len(fields) >= 1 {
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			hs.Load1m = v
		}
	}
	mem := ParseProcMeminfoDetailed(sections["MEMINFO"])
	hs.MemTotalMiB = float64(mem.MemTotalKiB) / 1024.0
	hs.MemAvailableMiB = float64(mem.MemAvailableKiB) / 1024.0
	hs.CachedMiB = float64(mem.CachedKiB) / 1024.0
	hs.BuffersMiB = float64(mem.BuffersKiB) / 1024.0
	hs.PageCacheMiB = float64(mem.CachedKiB+mem.BuffersKiB) / 1024.0
	hs.SlabMiB = float64(mem.SlabKiB) / 1024.0
	hs.SReclaimableMiB = float64(mem.SReclaimableKiB) / 1024.0
	hs.SUnreclaimMiB = float64(mem.SUnreclaimKiB) / 1024.0

	hs.CPUPSI = ParseProcPSI(sections["PSI_CPU"])
	hs.MemoryPSI = ParseProcPSI(sections["PSI_MEM"])
	hs.IOPSI = ParseProcPSI(sections["PSI_IO"])
	hs.OOMKillTotal = ParseProcVmstatOOMKill(sections["VMSTAT"])

	if lines := nonEmptyLines(sections["CONNTRACK"]); len(lines) >= 2 {
		if strings.EqualFold(lines[0], "unavailable") || strings.EqualFold(lines[1], "unavailable") {
			hs.ConntrackUnavailable = true
		} else {
			cnt, err1 := strconv.ParseInt(lines[0], 10, 64)
			mx, err2 := strconv.ParseInt(lines[1], 10, 64)
			if err1 != nil || err2 != nil {
				hs.ConntrackUnavailable = true
			} else {
				hs.ConntrackCount = cnt
				hs.ConntrackMax = mx
			}
		}
	}
	if fields := strings.Fields(sections["FILENR"]); len(fields) >= 1 {
		hs.OpenFDs, _ = strconv.ParseInt(fields[0], 10, 64)
	}
	if freeBytes, err := strconv.ParseInt(strings.TrimSpace(sections["STATEROOT_FREE"]), 10, 64); err == nil && freeBytes > 0 {
		hs.StateRootFreeGiB = float64(freeBytes) / float64(bytesPerGiB)
	}
	hs.ActiveUblkDevices, _ = strconv.Atoi(strings.TrimSpace(sections["UBLK_COUNT"]))
	hs.FirecrackerPIDCount, _ = strconv.Atoi(strings.TrimSpace(sections["FC_PIDS"]))

	return hs
}

// ParsePodCgroupOutput parses the sectioned shell output produced by sampleFastletPodCgroups.
func ParsePodCgroupOutput(raw string) PodCgroupSample {
	sections := splitSections(raw)
	var cg PodCgroupSample
	if b, err := strconv.ParseInt(strings.TrimSpace(sections["MEM_CUR"]), 10, 64); err == nil && b > 0 {
		cg.MemoryCurrentMiB = float64(b) / float64(bytesPerMiB)
	}
	memMaxStr := strings.TrimSpace(sections["MEM_MAX"])
	if memMaxStr != "" && memMaxStr != "max" {
		if b, err := strconv.ParseInt(memMaxStr, 10, 64); err == nil && b > 0 {
			cg.MemoryMaxMiB = float64(b) / float64(bytesPerMiB)
		}
	}
	cg.MemoryEvents = ParseCgroupMemoryEvents(sections["MEM_EVENTS"])
	cg.MemoryPSI = ParseProcPSI(sections["MEM_PSI"])
	cg.CPUPSI = ParseProcPSI(sections["CPU_PSI"])
	return cg
}

// ParseProcStat parses the first "cpu ..." line from /proc/stat.
func ParseProcStat(raw string) (procStatSample, bool) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var s procStatSample
		s.user, _ = strconv.ParseUint(fields[1], 10, 64)
		s.nice, _ = strconv.ParseUint(fields[2], 10, 64)
		s.system, _ = strconv.ParseUint(fields[3], 10, 64)
		s.idle, _ = strconv.ParseUint(fields[4], 10, 64)
		if len(fields) > 5 {
			s.iowait, _ = strconv.ParseUint(fields[5], 10, 64)
		}
		if len(fields) > 6 {
			s.irq, _ = strconv.ParseUint(fields[6], 10, 64)
		}
		if len(fields) > 7 {
			s.softirq, _ = strconv.ParseUint(fields[7], 10, 64)
		}
		if len(fields) > 8 {
			s.steal, _ = strconv.ParseUint(fields[8], 10, 64)
		}
		return s, true
	}
	return procStatSample{}, false
}

func computeCPUUtilDelta(prev, cur procStatSample, out *HostSample) {
	dt := cur.total() - prev.total()
	if dt == 0 || cur.total() < prev.total() {
		return
	}
	dIdle := cur.idle - prev.idle
	dIOWait := cur.iowait - prev.iowait
	dUser := (cur.user + cur.nice) - (prev.user + prev.nice)
	dSys := (cur.system + cur.irq + cur.softirq) - (prev.system + prev.irq + prev.softirq)
	dSteal := cur.steal - prev.steal

	totalF := float64(dt)
	busy := dt - dIdle - dIOWait
	if busy > dt {
		busy = 0
	}
	out.CPUUtilPct = (float64(busy) / totalF) * 100.0
	out.CPUUserPct = (float64(dUser) / totalF) * 100.0
	out.CPUSysPct = (float64(dSys) / totalF) * 100.0
	out.CPUIOWaitPct = (float64(dIOWait) / totalF) * 100.0
	out.CPUStealPct = (float64(dSteal) / totalF) * 100.0
}

// ProcMeminfoDetailed holds parsed Linux /proc/meminfo fields in KiB.
type ProcMeminfoDetailed struct {
	MemTotalKiB     int64
	MemAvailableKiB int64
	CachedKiB       int64
	BuffersKiB      int64
	SlabKiB         int64
	SReclaimableKiB int64
	SUnreclaimKiB   int64
}

// ParseProcMeminfoDetailed extracts MemTotal, MemAvailable, Cached, Buffers,
// Slab, SReclaimable, and SUnreclaim from /proc/meminfo in KiB.
func ParseProcMeminfoDetailed(raw string) ProcMeminfoDetailed {
	var m ProcMeminfoDetailed
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			m.MemTotalKiB = val
		case "MemAvailable":
			m.MemAvailableKiB = val
		case "Cached":
			m.CachedKiB = val
		case "Buffers":
			m.BuffersKiB = val
		case "Slab":
			m.SlabKiB = val
		case "SReclaimable":
			m.SReclaimableKiB = val
		case "SUnreclaim":
			m.SUnreclaimKiB = val
		}
	}
	return m
}

// ParseProcMeminfo extracts MemAvailable and page cache (Cached + Buffers) in KiB.
func ParseProcMeminfo(raw string) (memAvailableKiB int64, pageCacheKiB int64) {
	m := ParseProcMeminfoDetailed(raw)
	return m.MemAvailableKiB, m.CachedKiB + m.BuffersKiB
}

// TakeHostMemoryAndProcessSnapshot collects a single synchronized snapshot of
// host /proc/meminfo (including Slab/SReclaimable/SUnreclaim), all Firecracker
// process /proc/<pid>/smaps_rollup PSS/RSS totals, daemon RSS (fsb-fastlet,
// egress, overlaybd-ublkd, fastlet-proxy), and host CPU/load/PSI across all
// running firecracker-runtime pods (executing inside janitor with hostPID).
func (s *LoadSampler) TakeHostMemoryAndProcessSnapshot(ctx context.Context) (*HostMemoryAndProcessSnapshot, error) {
	if s == nil || s.clients == nil || s.clients.Clientset == nil || s.clients.Execer == nil {
		return nil, fmt.Errorf("load sampler kubernetes/exec clients not configured")
	}
	rtPods, err := listFirecrackerRuntimePods(ctx, s.clients.Clientset, s.cfg.SystemNamespace, s.cfg.ResourceNamespace)
	if err != nil {
		return nil, err
	}
	if len(rtPods) == 0 {
		return nil, fmt.Errorf("no running firecracker-runtime pods found")
	}

	const script = `
echo "===STAT==="
head -n 1 /proc/stat 2>/dev/null || true
echo "===LOADAVG==="
cat /proc/loadavg 2>/dev/null || true
echo "===MEMINFO==="
grep -E '^(MemTotal|MemAvailable|Cached|Buffers|Slab|SReclaimable|SUnreclaim):' /proc/meminfo 2>/dev/null || true
echo "===PSI_CPU==="
cat /proc/pressure/cpu 2>/dev/null || true
echo "===PSI_MEM==="
cat /proc/pressure/memory 2>/dev/null || true
echo "===FC_SMAPS==="
fc_cnt=0
fc_pss=0
fc_rss=0
for comm in /proc/[0-9]*/comm; do
  [ -r "$comm" ] || continue
  name=$(cat "$comm" 2>/dev/null || true)
  [ "$name" = "firecracker" ] || continue
  dir=${comm%/comm}
  fc_cnt=$((fc_cnt + 1))
  if [ -r "$dir/smaps_rollup" ]; then
    vals=$(awk '/^Rss:/{r=$2} /^Pss:/{p=$2} END{printf "%d %d", p+0, r+0}' "$dir/smaps_rollup" 2>/dev/null || echo "0 0")
    p=${vals%% *}
    r=${vals##* }
    fc_pss=$((fc_pss + p))
    fc_rss=$((fc_rss + r))
  fi
done
echo "fc_pid_count=$fc_cnt"
echo "fc_pss_kib=$fc_pss"
echo "fc_rss_kib=$fc_rss"
echo "===DAEMON_RSS==="
fastlet_rss=0
egress_rss=0
ublkd_rss=0
proxy_rss=0
for comm in /proc/[0-9]*/comm; do
  [ -r "$comm" ] || continue
  name=$(cat "$comm" 2>/dev/null || true)
  dir=${comm%/comm}
  case "$name" in
    fsb-fastlet|fastlet)
      r=$(awk '/^VmRSS:/{print $2; exit}' "$dir/status" 2>/dev/null || echo 0)
      fastlet_rss=$((fastlet_rss + ${r:-0}))
      ;;
    egress)
      r=$(awk '/^VmRSS:/{print $2; exit}' "$dir/status" 2>/dev/null || echo 0)
      egress_rss=$((egress_rss + ${r:-0}))
      ;;
    overlaybd-ublkd|ublkd)
      r=$(awk '/^VmRSS:/{print $2; exit}' "$dir/status" 2>/dev/null || echo 0)
      ublkd_rss=$((ublkd_rss + ${r:-0}))
      ;;
    fastlet-proxy|fsb-fastlet-pro)
      r=$(awk '/^VmRSS:/{print $2; exit}' "$dir/status" 2>/dev/null || echo 0)
      proxy_rss=$((proxy_rss + ${r:-0}))
      ;;
  esac
done
echo "fastlet_rss_kib=$fastlet_rss"
echo "egress_rss_kib=$egress_rss"
echo "ublkd_rss_kib=$ublkd_rss"
echo "proxy_rss_kib=$proxy_rss"
`

	type nodeRes struct {
		sample *nodeMemProcSample
		err    error
	}
	results := make([]nodeRes, len(rtPods))
	sem := make(chan struct{}, maxConcurrentNodeProbes)
	var wg sync.WaitGroup
	for i := range rtPods {
		wg.Add(1)
		go func(idx int, p *corev1.Pod) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nodeName := p.Spec.NodeName
			if nodeName == "" {
				nodeName = p.Name
			}
			container := pickRuntimeContainer(p, DefaultJanitorContainer, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
			stdout, stderr, execErr := execCommand(ctx, s.clients.Execer, p.Namespace, p.Name, container, []string{"/bin/sh", "-c", script}, s.cfg.ExecTimeout)
			if execErr != nil {
				results[idx] = nodeRes{err: fmt.Errorf("node %s memory/process snapshot: %w (%s)", nodeName, execErr, strings.TrimSpace(stderr))}
				return
			}
			parsed := ParseHostMemoryAndProcessOutput(nodeName, stdout)
			if parsed.cpuStat != nil {
				s.mu.Lock()
				prev, ok := s.prevCPUStat[nodeName]
				s.prevCPUStat[nodeName] = *parsed.cpuStat
				s.mu.Unlock()
				if ok {
					var tmp HostSample
					computeCPUUtilDelta(prev, *parsed.cpuStat, &tmp)
					parsed.CPUUtilPct = tmp.CPUUtilPct
				}
			}
			results[idx] = nodeRes{sample: &parsed}
		}(i, &rtPods[i])
	}
	wg.Wait()

	snap := &HostMemoryAndProcessSnapshot{Timestamp: time.Now().UTC()}
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if r.sample == nil {
			continue
		}
		ns := r.sample
		snap.NodeCount++
		snap.MemTotalMiB += ns.MemTotalMiB
		snap.MemAvailableMiB += ns.MemAvailableMiB
		snap.CachedMiB += ns.CachedMiB
		snap.BuffersMiB += ns.BuffersMiB
		snap.SlabMiB += ns.SlabMiB
		snap.SReclaimableMiB += ns.SReclaimableMiB
		snap.SUnreclaimMiB += ns.SUnreclaimMiB
		snap.FCPIDCount += ns.FCPIDCount
		snap.FCPssTotalMiB += ns.FCPssTotalMiB
		snap.FCRssTotalMiB += ns.FCRssTotalMiB
		snap.FastletRSSMiB += ns.FastletRSSMiB
		snap.EgressRSSMiB += ns.EgressRSSMiB
		snap.UblkdRSSMiB += ns.UblkdRSSMiB
		snap.ProxyRSSMiB += ns.ProxyRSSMiB
		if ns.CPUUtilPct > snap.CPUUtilPct {
			snap.CPUUtilPct = ns.CPUUtilPct
		}
		if ns.Load1m > snap.Load1m {
			snap.Load1m = ns.Load1m
		}
		if ns.CPUPSI.SomeAvg60 > snap.CPUPressureSomeAvg60 {
			snap.CPUPressureSomeAvg60 = ns.CPUPSI.SomeAvg60
		}
		if ns.MemoryPSI.FullAvg60 > snap.MemPressureFullAvg60 {
			snap.MemPressureFullAvg60 = ns.MemoryPSI.FullAvg60
		}
	}
	if snap.NodeCount == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no runtime nodes returned memory/process snapshots")
	}
	if snap.FCPIDCount > 0 {
		snap.FCPssAvgMiB = snap.FCPssTotalMiB / float64(snap.FCPIDCount)
		snap.FCRssAvgMiB = snap.FCRssTotalMiB / float64(snap.FCPIDCount)
	}

	// Prefer Prometheus daemon RSS when available and > 0, keeping /proc fallback.
	if s.scraper != nil {
		if promSnap, _ := s.scraper.Snapshot(ctx); promSnap != nil {
			if ms := promSnap.ByComponent[ComponentFSBFastlet]; ms != nil {
				if v := ms.Sum("process_resident_memory_bytes", nil) / float64(bytesPerMiB); v > 0 {
					snap.FastletRSSMiB = v
				}
			}
			if ms := promSnap.ByComponent[ComponentFSBFastletProxy]; ms != nil {
				if v := ms.Sum("process_resident_memory_bytes", nil) / float64(bytesPerMiB); v > 0 {
					snap.ProxyRSSMiB = v
				}
			}
			otelMS := promSnap.ByComponent[ComponentOTelCollector]
			if otelMS == nil {
				otelMS = promSnap.Merged
			}
			if otelMS != nil {
				if v, ok := firstNonZeroMetricSum(otelMS, "egress_process_memory_usage_bytes", "egress_process_memory_usage_bytes_bytes"); ok && v > 0 {
					snap.EgressRSSMiB = v / float64(bytesPerMiB)
				}
			}
		}
	}
	return snap, nil
}

type nodeMemProcSample struct {
	NodeName        string
	MemTotalMiB     float64
	MemAvailableMiB float64
	CachedMiB       float64
	BuffersMiB      float64
	SlabMiB         float64
	SReclaimableMiB float64
	SUnreclaimMiB   float64
	FCPIDCount      int
	FCPssTotalMiB   float64
	FCRssTotalMiB   float64
	FastletRSSMiB   float64
	EgressRSSMiB    float64
	UblkdRSSMiB     float64
	ProxyRSSMiB     float64
	CPUUtilPct      float64
	Load1m          float64
	CPUPSI          PSIMetrics
	MemoryPSI       PSIMetrics
	cpuStat         *procStatSample
}

// ParseHostMemoryAndProcessOutput parses the sectioned shell output produced by
// TakeHostMemoryAndProcessSnapshot.
func ParseHostMemoryAndProcessOutput(nodeName, raw string) nodeMemProcSample {
	out := nodeMemProcSample{NodeName: nodeName}
	sections := splitSections(raw)

	if st, ok := ParseProcStat(sections["STAT"]); ok {
		out.cpuStat = &st
	}
	if fields := strings.Fields(sections["LOADAVG"]); len(fields) >= 1 {
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			out.Load1m = v
		}
	}
	mem := ParseProcMeminfoDetailed(sections["MEMINFO"])
	out.MemTotalMiB = float64(mem.MemTotalKiB) / 1024.0
	out.MemAvailableMiB = float64(mem.MemAvailableKiB) / 1024.0
	out.CachedMiB = float64(mem.CachedKiB) / 1024.0
	out.BuffersMiB = float64(mem.BuffersKiB) / 1024.0
	out.SlabMiB = float64(mem.SlabKiB) / 1024.0
	out.SReclaimableMiB = float64(mem.SReclaimableKiB) / 1024.0
	out.SUnreclaimMiB = float64(mem.SUnreclaimKiB) / 1024.0

	out.CPUPSI = ParseProcPSI(sections["PSI_CPU"])
	out.MemoryPSI = ParseProcPSI(sections["PSI_MEM"])

	fcKV := parseKeyValues(sections["FC_SMAPS"])
	out.FCPIDCount = int(fcKV["fc_pid_count"])
	out.FCPssTotalMiB = float64(fcKV["fc_pss_kib"]) / 1024.0
	out.FCRssTotalMiB = float64(fcKV["fc_rss_kib"]) / 1024.0

	daemonKV := parseKeyValues(sections["DAEMON_RSS"])
	out.FastletRSSMiB = float64(daemonKV["fastlet_rss_kib"]) / 1024.0
	out.EgressRSSMiB = float64(daemonKV["egress_rss_kib"]) / 1024.0
	out.UblkdRSSMiB = float64(daemonKV["ublkd_rss_kib"]) / 1024.0
	out.ProxyRSSMiB = float64(daemonKV["proxy_rss_kib"]) / 1024.0

	return out
}

// ParseProcPSI parses Linux PSI format:
//
//	some avg10=0.12 avg60=0.34 avg300=0.56 total=12345
//	full avg10=0.01 avg60=0.02 avg300=0.03 total=678
func ParseProcPSI(raw string) PSIMetrics {
	var m PSIMetrics
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		for _, tok := range fields[1:] {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch kind + ":" + k {
			case "some:avg10":
				m.SomeAvg10 = f
			case "some:avg60":
				m.SomeAvg60 = f
			case "some:avg300":
				m.SomeAvg300 = f
			case "full:avg10":
				m.FullAvg10 = f
			case "full:avg60":
				m.FullAvg60 = f
			case "full:avg300":
				m.FullAvg300 = f
			}
		}
	}
	return m
}

// ParseProcVmstatOOMKill extracts the oom_kill counter from /proc/vmstat.
func ParseProcVmstatOOMKill(raw string) int64 {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "oom_kill" {
			v, _ := strconv.ParseInt(fields[1], 10, 64)
			return v
		}
	}
	return 0
}

// ParseCgroupMemoryEvents parses cgroup v2 memory.events (high, max, oom, oom_kill).
func ParseCgroupMemoryEvents(raw string) CgroupMemoryEvents {
	var ev CgroupMemoryEvents
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "high":
			ev.High = v
		case "max":
			ev.Max = v
		case "oom":
			ev.OOM = v
		case "oom_kill":
			ev.OOMKill = v
		}
	}
	return ev
}

func splitSections(raw string) map[string]string {
	out := make(map[string]string)
	var current string
	var buf strings.Builder
	flush := func() {
		if current != "" {
			out[current] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "===") && strings.HasSuffix(trimmed, "===") && len(trimmed) > 6 {
			flush()
			current = strings.TrimSuffix(strings.TrimPrefix(trimmed, "==="), "===")
			continue
		}
		if current != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return out
}

func nonEmptyLines(raw string) []string {
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}
