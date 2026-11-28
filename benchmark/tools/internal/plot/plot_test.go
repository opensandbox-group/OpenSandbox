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

package plot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

func TestRenderHTML_FullReportZeroCDNAndNoFirstExecCDF(t *testing.T) {
	rep := report.New(report.Provenance{
		RunID:             "phase2-full-test",
		ProfileName:       "fast-sandbox-overlaybd-ref",
		Runtime:           "fast-sandbox-overlaybd",
		OpenSandboxTag:    "v1.3.0-rc.3",
		OpenSandboxCommit: "ad653af12d21",
		Timestamp:         time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC),
	})
	rep.Environment.KubernetesVersion = "v1.36.2"
	rep.Environment.PoolMode = "ResourceAware"
	rep.LeakAudit = &report.LeakAudit{Passed: true}

	rep.Suites["schedule-burst"] = report.SuiteResult{
		Name:          "schedule-burst",
		Status:        "passed",
		TotalOps:      384,
		SucceededOps:  341,
		FailedOps:     43,
		ThroughputRPS: 25.9,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":    {Count: 341, MinMs: 82, P50Ms: 261, P90Ms: 1078, P95Ms: 1163, P99Ms: 3933, MaxMs: 3944},
			"t_first_ping": {Count: 341, MinMs: 92, P50Ms: 272, P90Ms: 1091, P95Ms: 1175, P99Ms: 3940, MaxMs: 3954},
			"t_first_exec": {Count: 341, MinMs: 309, P50Ms: 500, P90Ms: 1377, P95Ms: 1955, P99Ms: 5827, MaxMs: 5848},
		},
		Extra: map[string]any{
			"levels": []map[string]any{
				{
					"concurrency":               1,
					"attempts":                  128,
					"succeeded":                 128,
					"failed":                    0,
					"scheduling_throughput_rps": 2.59,
					"latencies": map[string]any{
						"t_api_ack": map[string]any{"p99_ms": 210.2},
					},
				},
				{
					"concurrency":               32,
					"attempts":                  128,
					"succeeded":                 128,
					"failed":                    0,
					"scheduling_throughput_rps": 25.90,
					"latencies": map[string]any{
						"t_api_ack": map[string]any{"p99_ms": 1247.4},
					},
				},
				{
					"concurrency":               64,
					"attempts":                  128,
					"succeeded":                 85,
					"failed":                    43,
					"scheduling_throughput_rps": 14.48,
					"latencies": map[string]any{
						"t_api_ack": map[string]any{"p99_ms": 3944.5},
					},
				},
			},
		},
	}

	rep.Suites["density-ramp"] = report.SuiteResult{
		Name:         "density-ramp",
		Status:       "passed",
		TotalOps:     230,
		SucceededOps: 230,
		Extra: map[string]any{
			"peak_active_sandboxes": 230,
			"steps": []map[string]any{
				{"target_count": 50, "live_sandboxes": 50, "ping_p99_ms": 7.59},
				{"target_count": 230, "live_sandboxes": 230, "ping_p99_ms": 5.21},
			},
		},
	}

	rep.Suites["dataplane-microbench"] = report.SuiteResult{
		Name:         "dataplane-microbench",
		Status:       "passed",
		TotalOps:     5,
		SucceededOps: 5,
		Extra: map[string]any{
			"cpu": map[string]any{
				"single":     map[string]any{"tool": "sysbench", "events_per_sec": 1800.0},
				"concurrent": map[string]any{"sandbox_count": 4, "mean_events_per_sec": 1650.0, "scaling_efficiency_ratio": 0.92},
			},
			"storage": map[string]any{
				"tool":                  "fio",
				"rand_read_4k_iops":     24000.0,
				"rand_write_4k_iops":    14000.0,
				"seq_read_1m_cold_mibs": 420.0,
				"seq_read_1m_hot_mibs":  2100.0,
				"seq_write_1m_mibs":     310.0,
			},
			"network": map[string]any{
				"tool":            "http-probe",
				"throughput_mbps": 18.5,
				"rtt_p50_ms":      2.1,
				"rtt_p99_ms":      6.4,
			},
		},
	}

	rep.Suites["noisy-neighbor"] = report.SuiteResult{
		Name:         "noisy-neighbor",
		Status:       "passed",
		TotalOps:     100,
		SucceededOps: 100,
		Extra: map[string]any{
			"aggressor_count":       2,
			"victim_count":          18,
			"baseline_p50_ms":       4.2,
			"baseline_p95_ms":       7.8,
			"baseline_p99_ms":       9.5,
			"noisy_p50_ms":          5.1,
			"noisy_p95_ms":          11.2,
			"noisy_p99_ms":          15.4,
			"p50_degradation_ratio": 1.21,
			"p99_degradation_ratio": 1.62,
			"failure_rate_delta":    0.0,
		},
	}

	hostMem := &HostMemoryProfile{
		TargetNode: "node-203",
		StepSnapshots: []HostMemorySnapshot{
			{Label: "step_50", FirecrackerCount: 50, HostMemAvailableDeltaMiBPerSandbox: 9.54, FirecrackerPSSMiBPerSandbox: 7.00, FirecrackerRSSMiBPerSandbox: 35.76},
			{Label: "step_230", FirecrackerCount: 230, HostMemAvailableDeltaMiBPerSandbox: 10.96, FirecrackerPSSMiBPerSandbox: 7.22, FirecrackerRSSMiBPerSandbox: 38.66},
		},
		PeakSnapshot: &HostMemorySnapshot{
			Label:                              "peak_230",
			FirecrackerCount:                   230,
			HostMemAvailableDeltaMiBPerSandbox: 10.96,
			FirecrackerPSSMiBPerSandbox:        7.22,
			FirecrackerRSSMiBPerSandbox:        38.66,
		},
	}

	outPath := filepath.Join(t.TempDir(), "report.html")
	err := WriteHTML(rep, outPath, Options{
		Title:      "Custom Phase 2 Report",
		HostMemory: hostMem,
	})
	require.NoError(t, err)

	raw, err := os.ReadFile(outPath)
	require.NoError(t, err)
	doc := string(raw)

	// Self-contained: zero external CDN links.
	require.NotContains(t, doc, "https://cdn")
	require.NotContains(t, doc, "http://cdn")
	require.NotContains(t, doc, "<script src=")
	require.NotContains(t, doc, "<link rel=\"stylesheet\"")

	// All 4 chart sections + knee annotation + custom title present.
	require.Contains(t, doc, "Custom Phase 2 Report")
	require.Contains(t, doc, `id="section-concurrency-knee"`)
	require.Contains(t, doc, "Zero-Error Knee: c=32")
	require.Contains(t, doc, `id="section-latency-cdf"`)
	require.Contains(t, doc, `id="section-density-memory"`)
	require.Contains(t, doc, `id="section-dataplane-microbench"`)
	require.Contains(t, doc, `id="section-noisy-neighbor"`)
	require.Contains(t, doc, "1.62x")
	require.Contains(t, doc, "10.96 MiB")

	// CDF must focus on t_api_ack and t_first_ping only (never t_first_exec), no OSEP-0034 label, Apache 2.0 header present.
	require.Contains(t, doc, "Copyright 2026 The OpenSandbox Authors")
	require.Contains(t, doc, "t_api_ack")
	require.Contains(t, doc, "t_first_ping")
	require.Contains(t, doc, "readiness_delta")
	require.Contains(t, doc, "events/s")
	require.NotContains(t, doc, "t_first_exec")
	require.NotContains(t, doc, "OSEP-0034")
	require.Contains(t, doc, `id="section-env-specs"`)
	require.Contains(t, doc, "@media print")
}

func TestFindZeroErrorKneeStopsAtFirstFailureAndKPIEdgeCases(t *testing.T) {
	// Contiguous zero-error prefix stops at c=32 even if c=128 has 0 failures.
	levels := []burstLevelData{
		{Concurrency: 1, Attempts: 64, Succeeded: 64, Failed: 0},
		{Concurrency: 32, Attempts: 64, Succeeded: 64, Failed: 0},
		{Concurrency: 64, Attempts: 64, Succeeded: 40, Failed: 24},
		{Concurrency: 128, Attempts: 64, Succeeded: 64, Failed: 0},
	}
	require.Equal(t, 1, FindZeroErrorKnee(levels))

	// LeakAudit FAIL shows actual failure reason; noisy <= 1.05x uses green accent & singular aggressor; blocked network shown.
	rep := report.New(report.Provenance{OpenSandboxTag: "release-1.3.0-rc.4", Runtime: "fast-sandbox-overlaybd", RunID: "tmp-run-123"})
	rep.LeakAudit = &report.LeakAudit{Passed: false, Failures: []string{"orphaned_ublk_devices = 1"}}
	rep.Suites["noisy-neighbor"] = report.SuiteResult{
		Name: "noisy-neighbor", Status: "passed",
		Extra: map[string]any{"aggressor_count": 1, "victim_count": 9, "p50_degradation_ratio": 0.99, "p99_degradation_ratio": 0.97},
	}
	rep.Suites["dataplane-microbench"] = report.SuiteResult{
		Name: "dataplane-microbench", Status: "failed",
		Extra: map[string]any{"network": map[string]any{"tool": "http-probe", "requests": 20, "succeeded": 0, "failed": 20}},
	}
	doc, err := RenderHTML(rep, Options{})
	require.NoError(t, err)
	require.Contains(t, doc, "OpenSandbox release-1.3.0-rc.4 Benchmark Report")
	require.Contains(t, doc, "1 / 2")
	require.Contains(t, doc, "orphaned_ublk_devices = 1")
	require.Contains(t, doc, "1 aggressor")
	require.Contains(t, doc, "kpi-card accent-green\"><div class=\"kpi-label\">Noisy P99 Degradation")
	require.Contains(t, doc, "0 / 20 OK (Blocked / Failed)")
}

func TestRenderHTML_GracefulDegradationMinimalReport(t *testing.T) {
	rep := report.New(report.Provenance{RunID: "minimal-run"})
	doc, err := RenderHTML(rep, Options{})
	require.NoError(t, err)
	require.Contains(t, doc, "<!DOCTYPE html>")
	require.Contains(t, doc, "--bg: #ffffff;")
	require.Contains(t, doc, "minimal-run")
	require.Contains(t, doc, `id="section-suite-overview"`)
	require.NotContains(t, doc, `id="section-concurrency-knee"`)
	require.NotContains(t, doc, `id="section-latency-cdf"`)
	require.NotContains(t, doc, `id="section-density-memory"`)
	require.NotContains(t, doc, `id="section-dataplane-microbench"`)
	require.NotContains(t, doc, `id="section-noisy-neighbor"`)
	require.NotContains(t, doc, `id="section-rootfs-only-restore"`)
}

func TestRenderHTML_RootfsOnlyRestoreSection(t *testing.T) {
	rep := report.New(report.Provenance{
		RunID:             "rootfs-plot-test",
		ProfileName:       "fast-sandbox-overlaybd-ref",
		Runtime:           "fast-sandbox-overlaybd",
		OpenSandboxTag:    "release-1.3.0-rc.4",
		OpenSandboxCommit: "73a3f31ba",
		Timestamp:         time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC),
	})
	rep.Suites["rootfs-only-restore"] = report.SuiteResult{
		Name:         "rootfs-only-restore",
		Status:       "passed",
		TotalOps:     6,
		SucceededOps: 6,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":                   {Count: 5, P50Ms: 110, P95Ms: 150, P99Ms: 170},
			"t_first_ping":                {Count: 5, P50Ms: 530, P95Ms: 650, P99Ms: 690},
			"snapshot_create":             {Count: 1, P50Ms: 410, P95Ms: 410, P99Ms: 410},
			"source_pause_stall":          {Count: 1, P50Ms: 48, P95Ms: 48, P99Ms: 48},
			"warm_materialize_first_ping": {Count: 1, P50Ms: 505, P95Ms: 505, P99Ms: 505},
			"cold_materialize_first_ping": {Count: 1, P50Ms: 675, P95Ms: 675, P99Ms: 675},
		},
		Extra: map[string]any{
			"dirty_upper_tiers": []map[string]any{
				{
					"dirty_upper_mib":       0,
					"snapshot_create_ms":    410.0,
					"source_pause_stall_ms": 48.0,
					"warm_first_ping_ms":    505.0,
					"cold_first_ping_ms":    675.0,
					"cold_cache_evicted":    true,
				},
			},
			"fanout": []map[string]any{
				{
					"concurrency": 8,
					"succeeded":   8,
					"total":       8,
					"t_first_ping": map[string]any{
						"count": 8, "p50_ms": 570.0, "p95_ms": 620.0, "p99_ms": 640.0,
					},
					"guest_boot_gap": map[string]any{
						"count": 8, "p50_ms": 420.0, "p95_ms": 460.0, "p99_ms": 480.0,
					},
				},
			},
			"cross_node": map[string]any{
				"before_node":   "node-203",
				"after_node":    "node-204",
				"cross_node_ok": true,
				"first_ping_ms": 712.0,
			},
			"resource_overrides": []map[string]any{
				{
					"vcpus":               1,
					"memory_mib":          1024,
					"observed_vcpus":      1,
					"observed_memory_mib": 982.0,
					"first_ping_ms":       520.0,
					"boot_gap_ms":         395.0,
					"override_verified":   true,
					"ok":                  true,
				},
			},
		},
	}

	doc, err := RenderHTML(rep, Options{})
	require.NoError(t, err)
	require.Contains(t, doc, `id="section-rootfs-only-restore"`)
	require.Contains(t, doc, "RootfsOnly Snapshot &amp; Cold-Boot Restore (FSEP-0001)")
	require.Contains(t, doc, "48.0 ms")
	require.Contains(t, doc, "PASS (712.0 ms)")
	require.Contains(t, doc, "1 / 1 Verified")
	require.Contains(t, doc, "c=8")
	require.Contains(t, doc, "Cold 0 MiB")
	require.NotContains(t, doc, "OSEP-0034")
	require.NotContains(t, doc, "t_first_exec")

	// When cold_cache_evicted is false, HTML plot must clearly label the restore as No-Evict.
	tiers := rep.Suites["rootfs-only-restore"].Extra["dirty_upper_tiers"].([]map[string]any)
	tiers[0]["cold_cache_evicted"] = false
	noEvictDoc, err := RenderHTML(rep, Options{})
	require.NoError(t, err)
	require.Contains(t, noEvictDoc, "No-Evict 0 MiB")
	require.Contains(t, noEvictDoc, "Warm vs No-Evict Restore")
	require.NotContains(t, noEvictDoc, "Cold 0 MiB")
}

func TestRenderCompareHTML_FullAndGracefulDegradation(t *testing.T) {
	base := report.New(report.Provenance{
		RunID:             "base-rc3",
		OpenSandboxTag:    "v1.3.0-rc.3",
		OpenSandboxCommit: "ad653af12d21",
		Runtime:           "fast-sandbox-overlaybd",
	})
	base.LeakAudit = &report.LeakAudit{Passed: true}
	base.Suites["schedule-burst"] = report.SuiteResult{
		Name: "schedule-burst", Status: "passed", TotalOps: 128, SucceededOps: 128, ThroughputRPS: 20.0,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":    {Count: 128, MinMs: 90, P50Ms: 180, P90Ms: 320, P95Ms: 360, P99Ms: 420, MaxMs: 480},
			"t_first_ping": {Count: 128, MinMs: 100, P50Ms: 195, P90Ms: 340, P95Ms: 380, P99Ms: 440, MaxMs: 500},
			"t_first_exec": {Count: 128, MinMs: 300, P50Ms: 500, P95Ms: 800, P99Ms: 900, MaxMs: 1000},
		},
		Extra: map[string]any{
			"levels": []map[string]any{
				{"concurrency": 1, "attempts": 64, "succeeded": 64, "failed": 0, "scheduling_throughput_rps": 3.0, "latencies": map[string]any{"t_api_ack": map[string]any{"count": 64, "p50_ms": 140.0, "p99_ms": 210.0}, "t_first_ping": map[string]any{"count": 64, "p50_ms": 150.0, "p99_ms": 220.0}}},
				{"concurrency": 32, "attempts": 64, "succeeded": 64, "failed": 0, "scheduling_throughput_rps": 20.0, "latencies": map[string]any{"t_api_ack": map[string]any{"count": 64, "p50_ms": 280.0, "p99_ms": 420.0}}},
			},
		},
	}
	base.Suites["density-ramp"] = report.SuiteResult{
		Name: "density-ramp", Status: "passed", TotalOps: 100, SucceededOps: 100,
		Extra: map[string]any{
			"steps": []map[string]any{
				{"live_sandboxes": 100, "ping_p99_ms": 6.5, "delta_mem_available_mib_per_sbx": 11.2, "fc_pss_mib_per_sbx": 7.4},
			},
		},
	}

	cand := report.New(report.Provenance{
		RunID:             "cand-v130",
		OpenSandboxTag:    "v1.3.0",
		OpenSandboxCommit: "f241dfe4e123",
		Runtime:           "fast-sandbox-overlaybd",
	})
	cand.LeakAudit = &report.LeakAudit{Passed: true}
	cand.Suites["schedule-burst"] = report.SuiteResult{
		Name: "schedule-burst", Status: "passed", TotalOps: 128, SucceededOps: 128, ThroughputRPS: 28.0,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":    {Count: 128, MinMs: 70, P50Ms: 140, P90Ms: 250, P95Ms: 280, P99Ms: 310, MaxMs: 360},
			"t_first_ping": {Count: 128, MinMs: 80, P50Ms: 150, P90Ms: 265, P95Ms: 295, P99Ms: 330, MaxMs: 380},
			"t_first_exec": {Count: 128, MinMs: 250, P50Ms: 400, P95Ms: 700, P99Ms: 800, MaxMs: 900},
		},
		Extra: map[string]any{
			"levels": []map[string]any{
				{"concurrency": 1, "attempts": 64, "succeeded": 64, "failed": 0, "scheduling_throughput_rps": 4.0, "latencies": map[string]any{"t_api_ack": map[string]any{"count": 64, "p50_ms": 110.0, "p99_ms": 160.0}, "t_first_ping": map[string]any{"count": 64, "p50_ms": 120.0, "p99_ms": 170.0}}},
				{"concurrency": 32, "attempts": 64, "succeeded": 64, "failed": 0, "scheduling_throughput_rps": 28.0, "latencies": map[string]any{"t_api_ack": map[string]any{"count": 64, "p50_ms": 210.0, "p99_ms": 310.0}}},
			},
		},
	}
	cand.Suites["density-ramp"] = report.SuiteResult{
		Name: "density-ramp", Status: "passed", TotalOps: 100, SucceededOps: 100,
		Extra: map[string]any{
			"steps": []map[string]any{
				{"live_sandboxes": 100, "ping_p99_ms": 4.2, "delta_mem_available_mib_per_sbx": 9.8, "fc_pss_mib_per_sbx": 6.9},
			},
		},
	}

	doc, err := RenderCompareHTML(base, cand, CompareOptions{})
	require.NoError(t, err)
	require.Contains(t, doc, "--bg: #ffffff;")
	require.NotContains(t, doc, "<script src=")
	require.NotContains(t, doc, "t_first_exec")
	require.Contains(t, doc, "v1.3.0-rc.3 (ad653af12d)")
	require.Contains(t, doc, "v1.3.0 (f241dfe4e1)")
	require.Contains(t, doc, `id="section-compare-concurrency"`)
	require.Contains(t, doc, `id="section-compare-latency"`)
	require.Contains(t, doc, `id="section-compare-density"`)
	require.Contains(t, doc, `id="section-compare-regression"`)
	require.Contains(t, doc, "NO REGRESSIONS")

	// Graceful degradation on minimal reports with no suites.
	minDoc, err := RenderCompareHTML(report.New(report.Provenance{RunID: "b0"}), report.New(report.Provenance{RunID: "c0"}), CompareOptions{})
	require.NoError(t, err)
	require.Contains(t, minDoc, `id="section-compare-regression"`)
	require.NotContains(t, minDoc, `id="section-compare-concurrency"`)
	require.NotContains(t, minDoc, `id="section-compare-rootfs-only"`)
}

func TestRenderCompareHTML_RootfsOnlyRestoreSection(t *testing.T) {
	base := report.New(report.Provenance{
		RunID:             "rootfs-base",
		OpenSandboxTag:    "release-1.3.0-rc.4",
		OpenSandboxCommit: "73a3f31ba",
		Runtime:           "fast-sandbox-overlaybd",
	})
	base.Suites["rootfs-only-restore"] = report.SuiteResult{
		Name: "rootfs-only-restore", Status: "passed", TotalOps: 37, SucceededOps: 37,
		Latencies: map[string]report.LatencyDist{
			"snapshot_create":             {Count: 3, P50Ms: 1040, P99Ms: 2050},
			"source_pause_stall":          {Count: 3, P50Ms: 60, P99Ms: 360},
			"warm_materialize_first_ping": {Count: 3, P50Ms: 900, P99Ms: 910},
			"cold_materialize_first_ping": {Count: 3, P50Ms: 1500, P99Ms: 1600},
			"t_api_ack":                   {Count: 34, P50Ms: 120, P99Ms: 240},
			"t_first_ping":                {Count: 34, P50Ms: 930, P99Ms: 1550},
		},
		Extra: map[string]any{
			"dirty_upper_tiers": []map[string]any{
				{"dirty_upper_mib": 0, "snapshot_create_ms": 1035.0, "source_pause_stall_ms": 23.0, "warm_first_ping_ms": 885.0, "cold_first_ping_ms": 1440.0, "cold_cache_evicted": true},
			},
			"fanout": []map[string]any{
				{"concurrency": 16, "succeeded": 16, "total": 16, "t_first_ping": map[string]any{"count": 16, "p50_ms": 945.0, "p99_ms": 980.0}},
			},
			"cross_node": map[string]any{"before_node": "node-204", "after_node": "node-203", "cross_node_ok": true, "first_ping_ms": 930.0},
		},
	}

	cand := report.New(report.Provenance{
		RunID:             "rootfs-cand",
		OpenSandboxTag:    "release-1.3.0-rc.5",
		OpenSandboxCommit: "abcdef123",
		Runtime:           "fast-sandbox-overlaybd",
	})
	cand.Suites["rootfs-only-restore"] = report.SuiteResult{
		Name: "rootfs-only-restore", Status: "passed", TotalOps: 37, SucceededOps: 37,
		Latencies: map[string]report.LatencyDist{
			"snapshot_create":             {Count: 3, P50Ms: 850, P99Ms: 1650},
			"source_pause_stall":          {Count: 3, P50Ms: 42, P99Ms: 280},
			"warm_materialize_first_ping": {Count: 3, P50Ms: 780, P99Ms: 800},
			"cold_materialize_first_ping": {Count: 3, P50Ms: 1280, P99Ms: 1350},
			"t_api_ack":                   {Count: 34, P50Ms: 95, P99Ms: 180},
			"t_first_ping":                {Count: 34, P50Ms: 810, P99Ms: 1320},
		},
		Extra: map[string]any{
			"dirty_upper_tiers": []map[string]any{
				{"dirty_upper_mib": 0, "snapshot_create_ms": 840.0, "source_pause_stall_ms": 18.0, "warm_first_ping_ms": 775.0, "cold_first_ping_ms": 1260.0, "cold_cache_evicted": true},
			},
			"fanout": []map[string]any{
				{"concurrency": 16, "succeeded": 16, "total": 16, "t_first_ping": map[string]any{"count": 16, "p50_ms": 820.0, "p99_ms": 850.0}},
			},
			"cross_node": map[string]any{"before_node": "node-204", "after_node": "node-203", "cross_node_ok": true, "first_ping_ms": 815.0},
		},
	}

	doc, err := RenderCompareHTML(base, cand, CompareOptions{})
	require.NoError(t, err)
	require.Contains(t, doc, `id="section-compare-rootfs-only"`)
	require.Contains(t, doc, "RootfsOnly Snapshot &amp; Cold-Boot Restore Comparison (FSEP-0001)")
	require.Contains(t, doc, "Dirty-Upper Tier Breakdown")
	require.Contains(t, doc, "Concurrent Fan-Out First Ping")
	require.Contains(t, doc, "Cross-Node Cold Boot First Ping")
	require.Contains(t, doc, "PASS (815.0 ms)")
	require.Contains(t, doc, "c=16")
	require.NotContains(t, doc, "OSEP-0034")
	require.NotContains(t, doc, "t_first_exec")
}

func TestRenderRolloutDisruptionHTMLAndCompare(t *testing.T) {
	makeRolloutRep := func(runID, tag string, outageMs, succPct float64, alive int) *report.Report {
		rep := report.New(report.Provenance{
			RunID:             runID,
			OpenSandboxTag:    tag,
			OpenSandboxCommit: "abc123456789",
			Runtime:           "fast-sandbox-overlaybd",
		})
		rep.Suites["rollout-disruption"] = report.SuiteResult{
			Name:         "rollout-disruption",
			Status:       "failed",
			TotalOps:     40,
			SucceededOps: 32,
			FailedOps:    8,
			Latencies: map[string]report.LatencyDist{
				"t_api_ack":    {Count: 32, MinMs: 50, P50Ms: 65, P90Ms: 90, P95Ms: 110, P99Ms: 140, MaxMs: 160},
				"t_first_ping": {Count: 32, MinMs: 60, P50Ms: 78, P90Ms: 105, P95Ms: 125, P99Ms: 155, MaxMs: 180},
			},
			Extra: map[string]any{
				"worst_max_outage_window_ms":      outageMs,
				"min_create_success_pct":          succPct,
				"total_survivor_drops":            4 - alive,
				"post_rollout_create_success_pct": 100.0,
				"phases": []map[string]any{
					{
						"phase":                "baseline",
						"kind":                 "baseline",
						"rollout_duration_s":   10.0,
						"total_creates":        20,
						"succeeded_creates":    20,
						"create_success_pct":   100.0,
						"max_outage_window_ms": 0.0,
						"survivor_total":       4,
						"survivor_alive_after": 4,
						"survivor_ping_total":  40,
						"survivor_ping_ok":     40,
						"survivor_ping_ok_pct": 100.0,
					},
					{
						"phase":                "pool_rollout",
						"kind":                 "disruption",
						"rollout_duration_s":   32.0,
						"total_creates":        20,
						"succeeded_creates":    12,
						"failed_creates":       8,
						"create_success_pct":   succPct,
						"max_outage_window_ms": outageMs,
						"survivor_total":       4,
						"survivor_alive_after": alive,
						"survivor_ping_total":  80,
						"survivor_ping_ok":     60,
						"survivor_ping_ok_pct": 75.0,
					},
				},
			},
		}
		return rep
	}

	base := makeRolloutRep("base-rollout", "rc.4-before", 15000.0, 60.0, 2)
	cand := makeRolloutRep("cand-rollout", "rc.4-after", 0.0, 100.0, 4)

	singleDoc, err := RenderHTML(base, Options{})
	require.NoError(t, err)
	require.Contains(t, singleDoc, `id="section-rollout-disruption"`)
	require.Contains(t, singleDoc, "Rollout Max Outage")
	require.Contains(t, singleDoc, "15000.0 ms")
	require.Contains(t, singleDoc, "pool_rollout")
	require.NotContains(t, singleDoc, "<script src=")
	require.NotContains(t, singleDoc, "OSEP-0034")
	require.NotContains(t, singleDoc, "t_first_exec")

	cmpDoc, err := RenderCompareHTML(base, cand, CompareOptions{})
	require.NoError(t, err)
	require.Contains(t, cmpDoc, `id="section-compare-rollout"`)
	require.Contains(t, cmpDoc, "-15000.0 ms")
	require.Contains(t, cmpDoc, "2/4 → 4/4")
	require.NotContains(t, cmpDoc, "<script src=")
	require.NotContains(t, cmpDoc, "OSEP-0034")
	require.NotContains(t, cmpDoc, "t_first_exec")
}
