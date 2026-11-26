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
	"math"
	"sync/atomic"
	"testing"
	"time"
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
