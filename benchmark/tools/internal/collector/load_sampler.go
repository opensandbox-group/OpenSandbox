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
	DefaultSamplerInterval = 2 * time.Second
	bytesPerMiB            = 1024 * 1024
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
	MemAvailableMiB       float64
	PageCacheMiB          float64
	CPUPSI                PSIMetrics
	MemoryPSI             PSIMetrics
	IOPSI                 PSIMetrics
	OOMKillTotal          int64
	StateRootFreeGiB      float64
	ConntrackCount        int64
	ConntrackMax          int64
	OpenFDs               int64
	ActiveUblkDevices     int
	FirecrackerPIDCount   int
	cpuStat               *procStatSample // internal for delta CPU utilization
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

// LatestHostSnapshot returns the most recent aggregated host metrics (useful for per-tier density frontier rows),
// with OOMKillTotal adjusted to the delta since the sampler's initial baseline reading.
func (s *LoadSampler) LatestHostSnapshot() (HostSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.samples) - 1; i >= 0; i-- {
		if len(s.samples[i].Hosts) > 0 {
			h := s.samples[i].Hosts[0]
			if base, ok := s.baselineOOM[h.NodeName]; ok {
				delta := h.OOMKillTotal - base
				if delta < 0 {
					delta = 0
				}
				h.OOMKillTotal = delta
			}
			return h, true
		}
	}
	return HostSample{}, false
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

	// 1. Sample host nodes via firecracker-runtime DaemonSet pods (hostPID + StateRoot mounted).
	if s.clients != nil && s.clients.Clientset != nil && s.clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, s.clients.Clientset, s.cfg.SystemNamespace, s.cfg.ResourceNamespace)
		if err != nil {
			sample.Warnings = append(sample.Warnings, fmt.Sprintf("list firecracker-runtime pods: %v", err))
		} else {
			for i := range rtPods {
				hs, warn := s.sampleHostNode(ctx, &rtPods[i])
				if warn != "" {
					sample.Warnings = append(sample.Warnings, warn)
				}
				if hs != nil {
					sample.Hosts = append(sample.Hosts, *hs)
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

	// 3. Probe fastlet sandbox-parent cgroup (podcgroup) if fastlet pods are present.
	if s.clients != nil && s.clients.Clientset != nil && s.clients.Execer != nil {
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
	container := pickRuntimeContainer(pod, DefaultFirecrackerAgentContainer, DefaultJanitorContainer, DefaultUblkdContainer)
	script := fmt.Sprintf(`
echo "===STAT==="
head -n 1 /proc/stat 2>/dev/null || true
echo "===LOADAVG==="
cat /proc/loadavg 2>/dev/null || true
echo "===MEMINFO==="
grep -E '^(MemAvailable|Cached|Buffers):' /proc/meminfo 2>/dev/null || true
echo "===PSI_CPU==="
cat /proc/pressure/cpu 2>/dev/null || true
echo "===PSI_MEM==="
cat /proc/pressure/memory 2>/dev/null || true
echo "===PSI_IO==="
cat /proc/pressure/io 2>/dev/null || true
echo "===VMSTAT==="
grep -E '^oom_kill ' /proc/vmstat 2>/dev/null || true
echo "===CONNTRACK==="
cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo 0
cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo 0
echo "===FILENR==="
cat /proc/sys/fs/file-nr 2>/dev/null || true
echo "===STATEROOT_FREE==="
df -B1 %q 2>/dev/null | awk 'NR==2 {print $4}' || echo 0
echo "===UBLK_COUNT==="
ls -1 /dev/ublkb* 2>/dev/null | wc -l | tr -d ' '
echo "===FC_PIDS==="
grep -l '^firecracker$' /proc/[0-9]*/comm 2>/dev/null | wc -l | tr -d ' '
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
		}
	}
	return &hs, ""
}

func (s *LoadSampler) sampleFastletPodCgroups(ctx context.Context) (*PodCgroupSample, string) {
	podList, err := s.clients.Clientset.CoreV1().Pods(s.cfg.ResourceNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Sprintf("list fastlet pods for podcgroup: %v", err)
	}
	var agg PodCgroupSample
	found := false
	script := `
CG="/sys/fs/cgroup"
for candidate in /sys/fs/cgroup/fast-sandbox /sys/fs/cgroup/sandboxes /sys/fs/cgroup; do
  if [ -f "$candidate/memory.current" ]; then
    CG="$candidate"
    break
  fi
done
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
`
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || !isFastletPod(pod) {
			continue
		}
		container := pickRuntimeContainer(pod, "fastlet", "fsb-fastlet")
		stdout, _, execErr := execCommand(ctx, s.clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", script}, s.cfg.ExecTimeout)
		if execErr != nil {
			continue
		}
		cg := ParsePodCgroupOutput(stdout)
		agg.MemoryCurrentMiB += cg.MemoryCurrentMiB
		agg.MemoryMaxMiB += cg.MemoryMaxMiB
		agg.MemoryEvents.High += cg.MemoryEvents.High
		agg.MemoryEvents.Max += cg.MemoryEvents.Max
		agg.MemoryEvents.OOM += cg.MemoryEvents.OOM
		agg.MemoryEvents.OOMKill += cg.MemoryEvents.OOMKill
		if cg.MemoryPSI.SomeAvg60 > agg.MemoryPSI.SomeAvg60 {
			agg.MemoryPSI = cg.MemoryPSI
		}
		if cg.CPUPSI.SomeAvg60 > agg.CPUPSI.SomeAvg60 {
			agg.CPUPSI = cg.CPUPSI
		}
		found = true
	}
	if !found {
		return nil, ""
	}
	return &agg, ""
}

func (s *LoadSampler) populateComponentsFromProm(snap *PromSnapshot, sample *LoadSample) {
	now := snap.Timestamp
	for _, comp := range []string{
		ComponentFSBFastlet,
		ComponentEgress,
		ComponentFSBFastletProxy,
		ComponentFirecrackerRuntime,
		ComponentOpenSandboxServer,
		ComponentIngressGateway,
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
			s.mu.Lock()
			prev, ok := s.prevProcCPU[comp]
			s.prevProcCPU[comp] = procCPUReading{ts: now, cpuSeconds: cpuSec}
			s.mu.Unlock()
			if ok {
				dt := now.Sub(prev.ts).Seconds()
				if dt > 0 && cpuSec >= prev.cpuSeconds {
					cs.CPUCores = (cpuSec - prev.cpuSeconds) / dt
				}
			}
		}
		if v := ms.Sum("container_cpu_cfs_throttled_periods_total", nil); v > 0 {
			cs.NrThrottled = int64(v)
		}
		if v := ms.Sum("container_cpu_cfs_throttled_seconds_total", nil); v > 0 {
			cs.ThrottledUsec = int64(v * 1e6)
		}
		// Capture component-specific domain metrics when exposed.
		switch comp {
		case ComponentFSBFastlet:
			for state, val := range ms.SumByLabel("fast_sandbox_fastlet_admission_slots", "state") {
				if state != "" {
					cs.ExtraMetrics["admission_slots_"+state] = val
				}
			}
		case ComponentEgress:
			if v := ms.Sum("egress_dns_queries_total", nil); v > 0 {
				cs.ExtraMetrics["dns_queries_total"] = v
			}
			if v := ms.Sum("egress_nftables_rules", nil); v > 0 {
				cs.ExtraMetrics["nftables_rules"] = v
			}
			if v := ms.Sum("egress_mitm_active_connections", nil); v > 0 {
				cs.ExtraMetrics["mitm_active_connections"] = v
			}
		case ComponentFSBFastletProxy:
			if v := ms.Sum("fast_sandbox_sandbox_proxy_active_connections", nil); v > 0 {
				cs.ExtraMetrics["active_connections"] = v
			}
			if v := ms.Sum("fast_sandbox_sandbox_proxy_route_latency_seconds_count", nil); v > 0 {
				cs.ExtraMetrics["route_requests_total"] = v
			}
		case ComponentOpenSandboxServer:
			if v := ms.Sum("http_requests_in_progress", nil); v > 0 {
				cs.ExtraMetrics["requests_in_progress"] = v
			}
		case ComponentIngressGateway:
			if v := ms.Sum("ingress_wake_parked_requests", nil); v > 0 {
				cs.ExtraMetrics["wake_parked_requests"] = v
			}
		}
		sample.Components[comp] = cs
	}
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
		cpuUtil, load1m, memAvail, pageCache              []float64
		cpuPSISome10, cpuPSISome60, cpuPSISome300         []float64
		memPSISome10, memPSISome60, memPSIFull60          []float64
		ioPSISome60, ioPSIFull60                          []float64
		stateRootFree, conntrackRatio, openFDs            []float64
		ublkCounts, fcPIDCounts                           []float64
		maxOOMDeltaByNode                                 = make(map[string]int64)
	)
	for _, sm := range samples {
		for _, h := range sm.Hosts {
			cpuUtil = append(cpuUtil, h.CPUUtilPct)
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
			if h.ConntrackMax > 0 {
				conntrackRatio = append(conntrackRatio, float64(h.ConntrackCount)/float64(h.ConntrackMax))
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
	if len(cpuUtil) == 0 && len(warnings) == 0 {
		return nil
	}

	var totalOOMKills int64
	for _, d := range maxOOMDeltaByNode {
		totalOOMKills += d
	}

	metrics := map[string]any{
		"cpu_util_pct":               SummarizeStats(cpuUtil),
		"load_1m":                    SummarizeStats(load1m),
		"cpu_psi_some_avg10":         SummarizeStats(cpuPSISome10),
		"cpu_psi_some_avg60":         SummarizeStats(cpuPSISome60),
		"cpu_psi_some_avg300":        SummarizeStats(cpuPSISome300),
		"mem_psi_some_avg10":         SummarizeStats(memPSISome10),
		"mem_psi_some_avg60":         SummarizeStats(memPSISome60),
		"mem_psi_full_avg60":         SummarizeStats(memPSIFull60),
		"io_psi_some_avg60":          SummarizeStats(ioPSISome60),
		"io_psi_full_avg60":          SummarizeStats(ioPSIFull60),
		"host_oom_kills":             totalOOMKills,
		"active_ublk_devices":        SummarizeStats(ublkCounts),
		"firecracker_pid_count":      SummarizeStats(fcPIDCounts),
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
		cpus, rss, goroutines, fds []float64
		perSbxRSS, perSbxCPUMilli  []float64
		cgMemCur, cgMemMax         []float64
		maxThrottled               int64
		maxThrottledUsec           int64
		maxCGHigh, maxCGMax        int64
		maxCGOOM, maxCGOOMKill     int64
		hasPodCgroup               bool
		extraSeries                = make(map[string][]float64)
	)
	for _, sm := range samples {
		cs, ok := sm.Components[component]
		if !ok {
			continue
		}
		cpus = append(cpus, cs.CPUCores)
		rss = append(rss, cs.RSSMiB)
		if cs.Goroutines > 0 {
			goroutines = append(goroutines, cs.Goroutines)
		}
		if cs.OpenFDs > 0 {
			fds = append(fds, cs.OpenFDs)
		}
		if cs.NrThrottled > maxThrottled {
			maxThrottled = cs.NrThrottled
		}
		if cs.ThrottledUsec > maxThrottledUsec {
			maxThrottledUsec = cs.ThrottledUsec
		}
		if sm.ActiveSandboxes > 0 {
			if cs.RSSMiB > 0 {
				perSbxRSS = append(perSbxRSS, cs.RSSMiB/float64(sm.ActiveSandboxes))
			}
			if cs.CPUCores > 0 {
				perSbxCPUMilli = append(perSbxCPUMilli, (cs.CPUCores*1000.0)/float64(sm.ActiveSandboxes))
			}
		}
		if cs.PodCgroup != nil {
			hasPodCgroup = true
			cgMemCur = append(cgMemCur, cs.PodCgroup.MemoryCurrentMiB)
			if cs.PodCgroup.MemoryMaxMiB > 0 {
				cgMemMax = append(cgMemMax, cs.PodCgroup.MemoryMaxMiB)
			}
			if cs.PodCgroup.MemoryEvents.High > maxCGHigh {
				maxCGHigh = cs.PodCgroup.MemoryEvents.High
			}
			if cs.PodCgroup.MemoryEvents.Max > maxCGMax {
				maxCGMax = cs.PodCgroup.MemoryEvents.Max
			}
			if cs.PodCgroup.MemoryEvents.OOM > maxCGOOM {
				maxCGOOM = cs.PodCgroup.MemoryEvents.OOM
			}
			if cs.PodCgroup.MemoryEvents.OOMKill > maxCGOOMKill {
				maxCGOOMKill = cs.PodCgroup.MemoryEvents.OOMKill
			}
		}
		for k, v := range cs.ExtraMetrics {
			extraSeries[k] = append(extraSeries[k], v)
		}
	}
	if len(cpus) == 0 && len(rss) == 0 && !hasPodCgroup {
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
	if maxThrottled > 0 {
		summary.Metrics["nr_throttled"] = maxThrottled
	}
	if maxThrottledUsec > 0 {
		summary.Metrics["throttled_usec"] = maxThrottledUsec
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
				"high":     maxCGHigh,
				"max":      maxCGMax,
				"oom":      maxCGOOM,
				"oom_kill": maxCGOOMKill,
			},
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
	memAvailKiB, cacheKiB := ParseProcMeminfo(sections["MEMINFO"])
	hs.MemAvailableMiB = float64(memAvailKiB) / 1024.0
	hs.PageCacheMiB = float64(cacheKiB) / 1024.0

	hs.CPUPSI = ParseProcPSI(sections["PSI_CPU"])
	hs.MemoryPSI = ParseProcPSI(sections["PSI_MEM"])
	hs.IOPSI = ParseProcPSI(sections["PSI_IO"])
	hs.OOMKillTotal = ParseProcVmstatOOMKill(sections["VMSTAT"])

	if lines := nonEmptyLines(sections["CONNTRACK"]); len(lines) >= 2 {
		hs.ConntrackCount, _ = strconv.ParseInt(lines[0], 10, 64)
		hs.ConntrackMax, _ = strconv.ParseInt(lines[1], 10, 64)
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

// ParseProcMeminfo extracts MemAvailable and page cache (Cached + Buffers) in KiB.
func ParseProcMeminfo(raw string) (memAvailableKiB int64, pageCacheKiB int64) {
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
		case "MemAvailable":
			memAvailableKiB = val
		case "Cached", "Buffers":
			pageCacheKiB += val
		}
	}
	return memAvailableKiB, pageCacheKiB
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
