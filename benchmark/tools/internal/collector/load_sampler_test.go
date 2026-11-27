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
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestHostAndCgroupParsers(t *testing.T) {
	rawHost := `
===STAT===
cpu  1000 100 500 8000 200 50 50 100 0 0
===LOADAVG===
3.45 2.10 1.80 2/412 12345
===MEMINFO===
MemAvailable:    16777216 kB
Buffers:          524288 kB
Cached:          2097152 kB
===PSI_CPU===
some avg10=1.25 avg60=2.50 avg300=0.75 total=999
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
===PSI_MEM===
some avg10=0.50 avg60=1.00 avg300=0.25 total=100
full avg10=0.10 avg60=0.20 avg300=0.05 total=20
===PSI_IO===
some avg10=3.00 avg60=4.00 avg300=2.00 total=500
full avg10=1.50 avg60=2.00 avg300=1.00 total=250
===VMSTAT===
oom_kill 2
===CONNTRACK===
4096
65536
===FILENR===
12345	0	1048576
===STATEROOT_FREE===
107374182400
===UBLK_COUNT===
12
===FC_PIDS===
8
`
	hs := ParseHostProbeOutput("node-1", rawHost)
	if hs.Load1m != 3.45 {
		t.Fatalf("expected load1m=3.45, got %v", hs.Load1m)
	}
	if math.Abs(hs.MemAvailableMiB-16384.0) > 1e-6 {
		t.Fatalf("expected MemAvailableMiB=16384, got %v", hs.MemAvailableMiB)
	}
	if math.Abs(hs.PageCacheMiB-2560.0) > 1e-6 {
		t.Fatalf("expected PageCacheMiB=2560, got %v", hs.PageCacheMiB)
	}
	if hs.CPUPSI.SomeAvg60 != 2.50 || hs.MemoryPSI.FullAvg60 != 0.20 || hs.IOPSI.FullAvg60 != 2.00 {
		t.Fatalf("unexpected PSI values: cpu=%+v mem=%+v io=%+v", hs.CPUPSI, hs.MemoryPSI, hs.IOPSI)
	}
	if hs.OOMKillTotal != 2 || hs.ConntrackCount != 4096 || hs.ConntrackMax != 65536 {
		t.Fatalf("unexpected vmstat/conntrack: %+v", hs)
	}
	if hs.ActiveUblkDevices != 12 || hs.FirecrackerPIDCount != 8 {
		t.Fatalf("unexpected ublk/fc counts: %d/%d", hs.ActiveUblkDevices, hs.FirecrackerPIDCount)
	}
	if math.Abs(hs.StateRootFreeGiB-100.0) > 1e-6 {
		t.Fatalf("expected 100 GiB free, got %v", hs.StateRootFreeGiB)
	}

	rawCG := `
===MEM_CUR===
536870912
===MEM_MAX===
4294967296
===MEM_EVENTS===
low 0
high 3
max 1
oom 0
oom_kill 0
===MEM_PSI===
some avg10=0.40 avg60=0.80 avg300=0.20 total=10
full avg10=0.05 avg60=0.10 avg300=0.02 total=2
`
	cg := ParsePodCgroupOutput(rawCG)
	if math.Abs(cg.MemoryCurrentMiB-512.0) > 1e-6 || math.Abs(cg.MemoryMaxMiB-4096.0) > 1e-6 {
		t.Fatalf("unexpected podcgroup memory: %+v", cg)
	}
	if cg.MemoryEvents.High != 3 || cg.MemoryEvents.Max != 1 || cg.MemoryEvents.OOMKill != 0 {
		t.Fatalf("unexpected podcgroup events: %+v", cg.MemoryEvents)
	}
}

func TestLoadSamplerAggregationAndLifecycle(t *testing.T) {
	sampler := NewLoadSampler(DefaultConfig(), nil, 15*time.Millisecond)
	var tick atomic.Int64
	sampler.sampleFn = func(_ context.Context) LoadSample {
		n := tick.Add(1)
		return LoadSample{
			Timestamp:       time.Now().UTC(),
			ActiveSandboxes: 10,
			Hosts: []HostSample{{
				NodeName:            "node-1",
				CPUUtilPct:          float64(10 * n),
				Load1m:              float64(n),
				MemAvailableMiB:     float64(10000 - 100*n),
				CPUPSI:              PSIMetrics{SomeAvg60: float64(n) * 0.5},
				MemoryPSI:           PSIMetrics{FullAvg60: float64(n) * 0.1},
				OOMKillTotal:        1, // constant baseline -> 0 delta
				StateRootFreeGiB:    50.0,
				ConntrackCount:      1000,
				ConntrackMax:        10000,
				ActiveUblkDevices:   10,
				FirecrackerPIDCount: 10,
			}},
			Components: map[string]ComponentSample{
				ComponentFSBFastlet: {
					CPUCores:      0.5 * float64(n),
					RSSMiB:        200.0,
					NrThrottled:   n,
					Goroutines:    120,
					OpenFDs:       64,
					PodCgroup:     &PodCgroupSample{MemoryCurrentMiB: 2048, MemoryMaxMiB: 8192, MemoryEvents: CgroupMemoryEvents{High: 2}},
				},
				ComponentEgress: {
					CPUCores:     0.2,
					RSSMiB:       60.0,
					ExtraMetrics: map[string]float64{"dns_queries_total": float64(100 * n)},
				},
				ComponentFSBFastletProxy: {
					CPUCores: 0.3,
					RSSMiB:   80.0,
				},
			},
		}
	}

	ctx := context.Background()
	sampler.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	profiles := sampler.Stop()

	if tick.Load() < 2 {
		t.Fatalf("expected at least 2 background samples, got %d", tick.Load())
	}
	if profiles.Host == nil || profiles.Fastlet == nil || profiles.Egress == nil || profiles.FastletProxy == nil {
		t.Fatalf("expected populated host/fastlet/egress/fastlet_proxy profiles, got %+v", profiles)
	}
	if profiles.Host.Metrics["host_oom_kills"] != int64(0) {
		t.Fatalf("expected 0 delta host_oom_kills, got %v", profiles.Host.Metrics["host_oom_kills"])
	}
	if math.Abs(profiles.Fastlet.PerSandboxResidentMiB-20.0) > 1e-6 {
		t.Fatalf("expected 20 MiB/sandbox for fastlet, got %v", profiles.Fastlet.PerSandboxResidentMiB)
	}
	if profiles.Fastlet.Metrics["podcgroup"] == nil {
		t.Fatalf("expected podcgroup metrics in fastlet summary")
	}
	latest, ok := sampler.LatestHostSnapshot()
	if !ok {
		t.Fatalf("expected LatestHostSnapshot to return a sample")
	}
	if latest.OOMKillTotal != 0 {
		t.Fatalf("expected LatestHostSnapshot OOMKillTotal delta=0, got %d", latest.OOMKillTotal)
	}
}

type samplerTestHTTPDoer struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (f samplerTestHTTPDoer) Do(req *http.Request) (*http.Response, error) { return f.fn(req) }

type countingExecer struct {
	calls atomic.Int32
}

func (c *countingExecer) Exec(_ context.Context, _ runner.ExecRequest) error {
	c.calls.Add(1)
	return nil
}

func TestLoadSamplerPodCgroupFromPrometheusSkipsExec(t *testing.T) {
	promBody := `
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 209715200
# TYPE fast_sandbox_runtime_cgroup_memory_bytes gauge
fast_sandbox_runtime_cgroup_memory_bytes{sandbox_uid=""} 536870912
fast_sandbox_runtime_cgroup_memory_bytes{sandbox_uid="sbx-1"} 134217728
# TYPE fast_sandbox_runtime_cgroup_memory_limit_bytes gauge
fast_sandbox_runtime_cgroup_memory_limit_bytes{sandbox_uid=""} 4294967296
fast_sandbox_runtime_cgroup_memory_limit_bytes{sandbox_uid="sbx-1"} 1073741824
# TYPE fast_sandbox_runtime_cgroup_oom_kills_total counter
fast_sandbox_runtime_cgroup_oom_kills_total{sandbox_uid=""} 3
fast_sandbox_runtime_cgroup_oom_kills_total{sandbox_uid="sbx-1"} 1
# TYPE fast_sandbox_runtime_cgroup_memory_psi_some_avg10 gauge
fast_sandbox_runtime_cgroup_memory_psi_some_avg10{sandbox_uid=""} 2.75
fast_sandbox_runtime_cgroup_memory_psi_some_avg10{sandbox_uid="sbx-1"} 9.99
# TYPE fast_sandbox_runtime_cgroup_cpu_throttled_periods_total counter
fast_sandbox_runtime_cgroup_cpu_throttled_periods_total{sandbox_uid=""} 42
fast_sandbox_runtime_cgroup_cpu_throttled_periods_total{sandbox_uid="sbx-1"} 99
`
	cfg := DefaultConfig()
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fastlet-pool-0",
			Namespace: cfg.ResourceNamespace,
			Labels:    map[string]string{"app": "fast-sandbox", "component": "fastlet"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.10",
		},
	})
	execer := &countingExecer{}
	clients := &Clients{
		Clientset: cs,
		Execer:    execer,
		HTTPClient: samplerTestHTTPDoer{fn: func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(promBody)),
			}, nil
		}},
	}

	sampler := NewLoadSampler(cfg, clients, time.Second)
	sampler.collectOnce(context.Background())

	if execer.calls.Load() != 0 {
		t.Fatalf("expected zero pods/exec calls when Prometheus exposes podcgroup metrics, got %d", execer.calls.Load())
	}

	sampler.mu.Lock()
	if len(sampler.samples) != 1 {
		sampler.mu.Unlock()
		t.Fatalf("expected 1 sample, got %d", len(sampler.samples))
	}
	fastletComp := sampler.samples[0].Components[ComponentFSBFastlet]
	sampler.mu.Unlock()

	if fastletComp.PodCgroup == nil {
		t.Fatalf("expected PodCgroup to be populated from Prometheus")
	}
	if math.Abs(fastletComp.PodCgroup.MemoryCurrentMiB-512.0) > 1e-6 {
		t.Fatalf("expected MemoryCurrentMiB=512 (pool aggregate only), got %v", fastletComp.PodCgroup.MemoryCurrentMiB)
	}
	if math.Abs(fastletComp.PodCgroup.MemoryMaxMiB-4096.0) > 1e-6 {
		t.Fatalf("expected MemoryMaxMiB=4096, got %v", fastletComp.PodCgroup.MemoryMaxMiB)
	}
	if fastletComp.PodCgroup.MemoryEvents.OOMKill != 3 {
		t.Fatalf("expected OOMKill=3, got %d", fastletComp.PodCgroup.MemoryEvents.OOMKill)
	}
	if math.Abs(fastletComp.PodCgroup.MemoryPSI.SomeAvg10-2.75) > 1e-6 {
		t.Fatalf("expected MemoryPSI.SomeAvg10=2.75, got %v", fastletComp.PodCgroup.MemoryPSI.SomeAvg10)
	}
	if fastletComp.NrThrottled != 42 {
		t.Fatalf("expected NrThrottled=42, got %d", fastletComp.NrThrottled)
	}
}

func TestLatestHostSnapshotMultiNodeWorstCaseAggregation(t *testing.T) {
	sampler := NewLoadSampler(DefaultConfig(), nil, time.Second)

	// Baseline sample establishes per-node OOM baselines (node-a=2, node-b=5).
	sampler.RecordSample(LoadSample{
		Hosts: []HostSample{
			{NodeName: "node-a", MemAvailableMiB: 16000, StateRootFreeGiB: 80, OOMKillTotal: 2},
			{NodeName: "node-b", MemAvailableMiB: 16000, StateRootFreeGiB: 80, OOMKillTotal: 5},
		},
	})

	// Second sample: node-b is saturated (lower mem/disk, higher CPU/PSI/conntrack, +2 OOM kills),
	// while node-a has +1 OOM kill and higher IO PSI.
	sampler.RecordSample(LoadSample{
		Hosts: []HostSample{
			{
				NodeName:            "node-a",
				CPUUtilPct:          35.0,
				Load1m:              2.0,
				MemAvailableMiB:     12000.0,
				StateRootFreeGiB:    60.0,
				CPUPSI:              PSIMetrics{SomeAvg10: 1.0, SomeAvg60: 1.5},
				MemoryPSI:           PSIMetrics{FullAvg60: 0.2},
				IOPSI:               PSIMetrics{SomeAvg60: 4.5, FullAvg60: 2.2},
				ConntrackCount:      2000,
				ConntrackMax:        65536,
				OOMKillTotal:        3, // delta +1
				ActiveUblkDevices:   12,
				FirecrackerPIDCount: 10,
			},
			{
				NodeName:            "node-b",
				CPUUtilPct:          92.5,
				Load1m:              14.5,
				MemAvailableMiB:     1024.0,
				StateRootFreeGiB:    12.5,
				CPUPSI:              PSIMetrics{SomeAvg10: 8.0, SomeAvg60: 12.0},
				MemoryPSI:           PSIMetrics{FullAvg60: 5.5},
				IOPSI:               PSIMetrics{SomeAvg60: 1.0, FullAvg60: 0.5},
				ConntrackCount:      9500,
				ConntrackMax:        65536,
				OOMKillTotal:        7, // delta +2
				ActiveUblkDevices:   18,
				FirecrackerPIDCount: 20,
			},
		},
	})

	agg, ok := sampler.LatestHostSnapshot()
	if !ok {
		t.Fatalf("expected LatestHostSnapshot to succeed")
	}
	if agg.MemAvailableMiB != 1024.0 {
		t.Fatalf("expected min MemAvailableMiB=1024, got %v", agg.MemAvailableMiB)
	}
	if agg.StateRootFreeGiB != 12.5 {
		t.Fatalf("expected min StateRootFreeGiB=12.5, got %v", agg.StateRootFreeGiB)
	}
	if agg.CPUUtilPct != 92.5 || agg.Load1m != 14.5 {
		t.Fatalf("expected max CPUUtilPct=92.5 Load1m=14.5, got %v / %v", agg.CPUUtilPct, agg.Load1m)
	}
	if agg.CPUPSI.SomeAvg60 != 12.0 || agg.MemoryPSI.FullAvg60 != 5.5 || agg.IOPSI.SomeAvg60 != 4.5 || agg.IOPSI.FullAvg60 != 2.2 {
		t.Fatalf("unexpected worst-case PSI: cpu=%+v mem=%+v io=%+v", agg.CPUPSI, agg.MemoryPSI, agg.IOPSI)
	}
	if agg.ConntrackCount != 9500 {
		t.Fatalf("expected max ConntrackCount=9500, got %d", agg.ConntrackCount)
	}
	if agg.OOMKillTotal != 3 {
		t.Fatalf("expected summed OOM delta=3 (1+2), got %d", agg.OOMKillTotal)
	}
	if agg.ActiveUblkDevices != 30 || agg.FirecrackerPIDCount != 30 {
		t.Fatalf("expected summed ublk=30 fc=30, got %d / %d", agg.ActiveUblkDevices, agg.FirecrackerPIDCount)
	}
}

func TestLoadSamplerTick0CPUBiasAndPodCgroupBaselineAndBreakdown(t *testing.T) {
	sampler := NewLoadSampler(DefaultConfig(), nil, time.Second)
	// Tick 0: cpuDeltaPending=true should NOT inject 0.0 into CPU averages;
	// podcgroup cumulative events establish baseline (high=5, oom_kill=2, nr_throttled=10).
	sampler.RecordSample(LoadSample{
		ActiveSandboxes: 10,
		Hosts: []HostSample{
			{
				NodeName:        "node-1",
				CPUUtilPct:      0,
				cpuDeltaPending: true,
				Load1m:          1.0,
				MemAvailableMiB: 8000,
			},
			{
				// Partial probe failure on node-2 returns 0 for MemAvailableMiB / StateRootFreeGiB.
				NodeName:         "node-2",
				MemAvailableMiB:  0,
				StateRootFreeGiB: 0,
				cpuDeltaPending:  true,
			},
		},
		Components: map[string]ComponentSample{
			ComponentFSBFastlet: {
				CPUCores:        0,
				cpuDeltaPending: true,
				RSSMiB:          100,
				NrThrottled:     10,
				PodCgroup: &PodCgroupSample{
					MemoryCurrentMiB: 512,
					MemoryEvents:     CgroupMemoryEvents{High: 5, Max: 1, OOM: 2, OOMKill: 2},
					MemoryPSI:        PSIMetrics{SomeAvg10: 1.0, SomeAvg60: 2.0, FullAvg60: 0.5},
					CPUPSI:           PSIMetrics{SomeAvg10: 0.2, SomeAvg60: 0.4},
				},
			},
		},
	})
	// Tick 1: real CPU deltas and +1 podcgroup oom_kill, +3 high, +5 nr_throttled.
	sampler.RecordSample(LoadSample{
		ActiveSandboxes: 10,
		Hosts: []HostSample{
			{
				NodeName:         "node-1",
				CPUUtilPct:       60.0,
				CPUUserPct:       40.0,
				CPUSysPct:        15.0,
				CPUIOWaitPct:     3.0,
				CPUStealPct:      2.0,
				Load1m:           2.0,
				MemAvailableMiB:  7000,
				StateRootFreeGiB: 40.0,
			},
			{
				NodeName:         "node-2",
				CPUUtilPct:       60.0,
				CPUUserPct:       40.0,
				CPUSysPct:        15.0,
				CPUIOWaitPct:     3.0,
				CPUStealPct:      2.0,
				MemAvailableMiB:  0, // partial failure must be skipped
				StateRootFreeGiB: 0, // partial failure must be skipped
			},
		},
		Components: map[string]ComponentSample{
			ComponentFSBFastlet: {
				CPUCores:    2.0,
				RSSMiB:      120,
				NrThrottled: 15,
				PodCgroup: &PodCgroupSample{
					MemoryCurrentMiB: 600,
					MemoryEvents:     CgroupMemoryEvents{High: 8, Max: 1, OOM: 3, OOMKill: 3},
					MemoryPSI:        PSIMetrics{SomeAvg10: 3.0, SomeAvg60: 4.0, FullAvg60: 1.5},
					CPUPSI:           PSIMetrics{SomeAvg10: 0.6, SomeAvg60: 0.8},
				},
			},
		},
	})

	lp := sampler.Aggregate()
	if lp.Host == nil || lp.Fastlet == nil {
		t.Fatalf("expected non-nil host and fastlet summaries")
	}
	// Tick 0 cpuDeltaPending was skipped, so Host CPU avg must be 60.0 (not 30.0).
	if math.Abs(lp.Host.CPUCores.Avg-60.0) > 1e-6 {
		t.Fatalf("expected host CPU avg=60.0 without tick-0 zero bias, got %v", lp.Host.CPUCores.Avg)
	}
	if lp.Host.Metrics["cpu_user_pct"] == nil || lp.Host.Metrics["cpu_sys_pct"] == nil ||
		lp.Host.Metrics["cpu_iowait_pct"] == nil || lp.Host.Metrics["cpu_steal_pct"] == nil {
		t.Fatalf("expected host CPU breakdown metrics to be preserved: %+v", lp.Host.Metrics)
	}
	// Fastlet tick-0 CPU was skipped, so avg must be 2.0 (not 1.0).
	if math.Abs(lp.Fastlet.CPUCores.Avg-2.0) > 1e-6 {
		t.Fatalf("expected fastlet CPU avg=2.0 without tick-0 zero bias, got %v", lp.Fastlet.CPUCores.Avg)
	}
	if lp.Fastlet.Metrics["nr_throttled"] != int64(5) {
		t.Fatalf("expected nr_throttled delta=5, got %v", lp.Fastlet.Metrics["nr_throttled"])
	}
	cgMap := lp.Fastlet.Metrics["podcgroup"].(map[string]any)
	evMap := cgMap["memory_events"].(map[string]int64)
	if evMap["high"] != 3 || evMap["max"] != 0 || evMap["oom"] != 1 || evMap["oom_kill"] != 1 {
		t.Fatalf("expected baseline-subtracted podcgroup events {high:3 max:0 oom:1 oom_kill:1}, got %+v", evMap)
	}
	if cgMap["memory_psi_some_avg60"] == nil || cgMap["cpu_psi_some_avg60"] == nil {
		t.Fatalf("expected podcgroup PSI metrics to be preserved: %+v", cgMap)
	}
	if got := sampler.LatestPodCgroupOOMDelta(); got != 1 {
		t.Fatalf("expected LatestPodCgroupOOMDelta=1, got %d", got)
	}
	latest, ok := sampler.LatestHostSnapshot()
	if !ok || latest.MemAvailableMiB != 7000 || latest.StateRootFreeGiB != 40.0 {
		t.Fatalf("expected LatestHostSnapshot to skip 0 partial probe failures, got %+v", latest)
	}
}
