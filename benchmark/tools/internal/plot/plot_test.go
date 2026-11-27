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

	// CDF must focus on t_api_ack and t_first_ping only (never t_first_exec).
	require.Contains(t, doc, "t_api_ack")
	require.Contains(t, doc, "t_first_ping")
	require.NotContains(t, doc, "t_first_exec")
}

func TestRenderHTML_GracefulDegradationMinimalReport(t *testing.T) {
	rep := report.New(report.Provenance{RunID: "minimal-run"})
	doc, err := RenderHTML(rep, Options{})
	require.NoError(t, err)
	require.Contains(t, doc, "<!DOCTYPE html>")
	require.Contains(t, doc, "minimal-run")
	require.Contains(t, doc, `id="section-suite-overview"`)
	require.NotContains(t, doc, `id="section-concurrency-knee"`)
	require.NotContains(t, doc, `id="section-latency-cdf"`)
	require.NotContains(t, doc, `id="section-density-memory"`)
	require.NotContains(t, doc, `id="section-dataplane-microbench"`)
	require.NotContains(t, doc, `id="section-noisy-neighbor"`)
}
