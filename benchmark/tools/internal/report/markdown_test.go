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
