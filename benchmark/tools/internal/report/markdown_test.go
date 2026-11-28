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

package report

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRenderSummaryMarkdownDeterministicMetricsSections(t *testing.T) {
	rep := New(Provenance{
		RunID:       "run-det-1",
		ProfileName: "fast-sandbox-overlaybd-ref",
		Runtime:     "fast-sandbox-overlaybd",
		Timestamp:   time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
	})
	rep.TelemetryStatus = "degraded"
	rep.TelemetryMissing = []string{"nodes:list failed: forbidden"}
	rep.Suites["density-ramp"] = SuiteResult{
		Name:         "density-ramp",
		Status:       "failed",
		TotalOps:     10,
		SucceededOps: 8,
		FailedOps:    2,
		ErrorsByOperation: map[string]map[string]int{
			"soak_egress_0.50x": {"exit_6_dns": 2},
		},
		ErrorSamples: []ErrorSample{
			{Operation: "soak_egress_0.50x", SandboxID: "sbx-1", Code: "exit_6_dns", StderrHead: "Could not resolve host"},
		},
		StageAttribution: []StageAttribution{
			{SpanName: "client.api_ack", P50SelfMs: 80, P99SelfMs: 120, CriticalShare: 40.0, Note: "POST /v1/sandboxes"},
		},
		Extra: map[string]any{
			"steps": []map[string]any{
				{
					"target_count":                    100,
					"live_sandboxes":                  100,
					"ping_healthy":                    20,
					"ping_sampled":                    20,
					"ping_p99_ms":                     12.5,
					"delta_mem_available_mib_per_sbx": 85.4,
					"fc_pss_mib_per_sbx":              41.2,
					"fc_rss_mib_per_sbx":              62.8,
					"delta_slab_mib_per_sbx":          3.1,
					"fastlet_rss_mib":                 120.0,
					"egress_rss_mib":                  48.0,
					"ublkd_rss_mib":                   210.0,
					"host_cpu_util_pct":               35.5,
					"host_load_1m":                    4.2,
				},
			},
		},
	}
	rep.LeakAudit = &LeakAudit{
		Passed:                false,
		Incomplete:            true,
		BaselineCleanSlots:    64,
		CleanSlotsAfter:       60,
		LostCleanSlots:        4,
		NonCleanResidualSlots: 1,
		AdmissionSlotsByState: map[string]int{"clean": 60, "dirty": 1},
		Warnings:              []string{"node-1 probe failed"},
		Failures:              []string{"audit_probe_failed: node-1 probe failed"},
	}

	md := RenderSummaryMarkdown(rep)
	require.Contains(t, md, "**Telemetry Status**: `degraded`")
	require.Contains(t, md, "nodes:list failed: forbidden")
	require.Contains(t, md, "### Density Ramp Step Telemetry (`density-ramp`)")
	require.Contains(t, md, "85.40")
	require.Contains(t, md, "### Error Breakdown")
	require.Contains(t, md, "exit_6_dns")
	require.Contains(t, md, "Could not resolve host")
	require.Contains(t, md, "### P99 Bottleneck Attribution (`density-ramp`)")
	require.Contains(t, md, "## Leak & Stability Audit: FAIL (INCOMPLETE)")
	require.Contains(t, md, "baseline clean=`64`, after clean=`60`, lost=`4`, non-clean residual=`1`")
	require.Contains(t, md, "Warning: node-1 probe failed")
}

func TestRenderSummaryMarkdownRootfsOnlyRestore(t *testing.T) {
	rep := New(Provenance{
		RunID:       "run-rootfs-1",
		ProfileName: "fast-sandbox-overlaybd-ref",
		Runtime:     "fast-sandbox-overlaybd",
		Timestamp:   time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC),
	})
	rep.Suites["rootfs-only-restore"] = SuiteResult{
		Name:         "rootfs-only-restore",
		Status:       "passed",
		TotalOps:     7,
		SucceededOps: 7,
		Latencies: map[string]LatencyDist{
			"t_api_ack":                   {Count: 6, P50Ms: 120, P95Ms: 180, P99Ms: 210},
			"t_first_ping":                {Count: 6, P50Ms: 540, P95Ms: 680, P99Ms: 710},
			"snapshot_create":             {Count: 1, P50Ms: 420, P95Ms: 420, P99Ms: 420},
			"source_pause_stall":          {Count: 1, P50Ms: 55, P95Ms: 55, P99Ms: 55},
			"warm_materialize_first_ping": {Count: 1, P50Ms: 510, P95Ms: 510, P99Ms: 510},
			"cold_materialize_first_ping": {Count: 1, P50Ms: 690, P95Ms: 690, P99Ms: 690},
		},
		Extra: map[string]any{
			"dirty_upper_tiers": []map[string]any{
				{
					"dirty_upper_mib":        16,
					"snapshot_create_ms":     420.5,
					"source_pause_stall_ms":  55.2,
					"post_capture_source_ok": true,
					"warm_first_ping_ms":     510.0,
					"warm_boot_gap_ms":       390.0,
					"warm_marker_verified":   true,
					"cold_cache_evicted":     true,
					"cold_first_ping_ms":     690.0,
					"cold_boot_gap_ms":       520.0,
					"cold_marker_verified":   true,
				},
			},
			"fanout": []map[string]any{
				{
					"concurrency": 8,
					"succeeded":   8,
					"total":       8,
					"t_first_ping": map[string]any{
						"count": 8, "p50_ms": 580.0, "p95_ms": 640.0, "p99_ms": 660.0,
					},
					"guest_boot_gap": map[string]any{
						"count": 8, "p50_ms": 430.0, "p95_ms": 480.0, "p99_ms": 500.0,
					},
				},
			},
			"cross_node": map[string]any{
				"before_node":     "node-203",
				"after_node":      "node-204",
				"cross_node_ok":   true,
				"first_ping_ms":   730.5,
				"boot_gap_ms":     560.2,
				"marker_verified": true,
			},
			"resource_overrides": []map[string]any{
				{
					"vcpus":               1,
					"memory_mib":          1024,
					"observed_vcpus":      1,
					"observed_memory_mib": 985.5,
					"entrypoint":          []string{"sleep", "3600"},
					"entrypoint_verified": true,
					"first_ping_ms":       525.0,
					"boot_gap_ms":         400.0,
					"override_verified":   true,
					"ok":                  true,
				},
			},
		},
	}

	md := RenderSummaryMarkdown(rep)
	require.Contains(t, md, "### RootfsOnly Snapshot & Cold-Boot Restore (`rootfs-only-restore`)")
	require.Contains(t, md, "| 16 | 420.5 | 55.2 | true | 510.0 / 390.0 | 690.0 / 520.0 | true | true / true |")
	require.Contains(t, md, "#### Concurrent Fan-Out")
	require.Contains(t, md, "| 8 | 8 / 8 | 580.0 / 640.0 / 660.0 | 430.0 / 480.0 / 500.0 |")
	require.Contains(t, md, "#### Cross-Node Cold Boot")
	require.Contains(t, md, "`node-203` -> `node-204`")
	require.Contains(t, md, "#### Resource & Entrypoint Overrides")
	require.Contains(t, md, "| 1 / 1024 | 1 / 985.5 | `sleep 3600` (ok=true) | 525.0 / 400.0 | true | PASS |")
}

func TestRenderSummaryMarkdownSanitizesRootfsCells(t *testing.T) {
	rep := New(Provenance{
		RunID:       "run-rootfs-escape",
		ProfileName: "fast-sandbox-overlaybd-ref",
		Runtime:     "fast-sandbox-overlaybd",
		Timestamp:   time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC),
	})
	rep.Suites["rootfs-only-restore"] = SuiteResult{
		Name:   "rootfs-only-restore",
		Status: "failed",
		Extra: map[string]any{
			"cross_node": map[string]any{
				"before_node": "node-a",
				"after_node":  "node-b",
				"error":       "line1\nline2 | `code`",
			},
			"resource_overrides": []map[string]any{
				{
					"vcpus":      1,
					"memory_mib": 1024,
					"entrypoint": []string{"sh", "-c", "echo a | `id`"},
					"ok":         false,
					"error":      "HTTP 500:\nbody | with `backticks`",
				},
			},
		},
	}

	md := RenderSummaryMarkdown(rep)
	require.Contains(t, md, `- Error: line1 line2 \| 'code'`)
	require.Contains(t, md, "| `sh -c echo a \\| 'id'` |")
	require.Contains(t, md, `FAIL: HTTP 500: body \| with 'backticks'`)
}

func TestRenderSummaryMarkdownRolloutDisruption(t *testing.T) {
	rep := New(Provenance{
		RunID:       "run-rollout-1",
		ProfileName: "fast-sandbox-overlaybd-ref",
		Runtime:     "fast-sandbox-overlaybd",
		Timestamp:   time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
	})
	rep.Suites["rollout-disruption"] = SuiteResult{
		Name:         "rollout-disruption",
		Status:       "failed",
		TotalOps:     50,
		SucceededOps: 42,
		FailedOps:    8,
		Extra: map[string]any{
			"worst_max_outage_window_ms": 14200.5,
			"min_create_success_pct":     60.0,
			"total_survivor_drops":       2,
			"phases": []map[string]any{
				{
					"phase":                "baseline",
					"kind":                 "baseline",
					"rollout_duration_s":   10.0,
					"total_creates":        20,
					"succeeded_creates":    20,
					"create_success_pct":   100.0,
					"max_outage_window_ms": 0.0,
					"t_api_ack":            map[string]any{"count": 20, "p50_ms": 62.0, "p99_ms": 85.0},
					"t_first_ping":         map[string]any{"count": 20, "p50_ms": 74.0, "p99_ms": 96.0},
					"survivor_total":       4,
					"survivor_alive_after": 4,
					"survivor_ping_total":  80,
					"survivor_ping_ok":     80,
					"survivor_ping_ok_pct": 100.0,
				},
				{
					"phase":                "pool_rollout",
					"kind":                 "disruption",
					"scenario":             "pool_rollout",
					"rollout_duration_s":   35.2,
					"total_creates":        20,
					"succeeded_creates":    12,
					"failed_creates":       8,
					"create_success_pct":   60.0,
					"max_outage_window_ms": 14200.5,
					"t_api_ack":            map[string]any{"count": 12, "p50_ms": 70.0, "p99_ms": 120.0},
					"t_first_ping":         map[string]any{"count": 12, "p50_ms": 82.0, "p99_ms": 145.0},
					"survivor_total":       4,
					"survivor_alive_after": 2,
					"survivor_ping_total":  120,
					"survivor_ping_ok":     90,
					"survivor_ping_ok_pct": 75.0,
				},
			},
		},
	}

	md := RenderSummaryMarkdown(rep)
	require.Contains(t, md, "### Rollout Disruption Analysis (`rollout-disruption`)")
	require.Contains(t, md, "14200.5 ms")
	require.Contains(t, md, "`pool_rollout`")
	require.Contains(t, md, "2 / 4")
}
