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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAggregateCyclesAndRenderMarkdown(t *testing.T) {
	r1 := New(Provenance{RunID: "run-cycle-1", ProfileName: "ref", Runtime: "fast-sandbox-overlaybd"})
	r1.Suites["schedule-burst"] = SuiteResult{
		Name:          "schedule-burst",
		Status:        "passed",
		TotalOps:      20,
		SucceededOps:  20,
		ThroughputRPS: 10.0,
		Latencies: map[string]LatencyDist{
			"t_api_ack":    {Count: 20, P50Ms: 20, P95Ms: 40, P99Ms: 60},
			"t_first_ping": {Count: 20, P50Ms: 100, P95Ms: 200, P99Ms: 300},
		},
	}
	r1.LeakAudit = &LeakAudit{Passed: true}

	r2 := New(Provenance{RunID: "run-cycle-2", ProfileName: "ref", Runtime: "fast-sandbox-overlaybd"})
	r2.Suites["schedule-burst"] = SuiteResult{
		Name:          "schedule-burst",
		Status:        "passed",
		TotalOps:      20,
		SucceededOps:  20,
		ThroughputRPS: 16.0,
		Latencies: map[string]LatencyDist{
			"t_api_ack":    {Count: 20, P50Ms: 30, P95Ms: 50, P99Ms: 70},
			"t_first_ping": {Count: 20, P50Ms: 120, P95Ms: 240, P99Ms: 360},
		},
	}
	r2.LeakAudit = &LeakAudit{Passed: true}

	r3 := New(Provenance{RunID: "run-cycle-3", ProfileName: "ref", Runtime: "fast-sandbox-overlaybd"})
	r3.Suites["schedule-burst"] = SuiteResult{
		Name:          "schedule-burst",
		Status:        "failed",
		TotalOps:      20,
		SucceededOps:  18,
		FailedOps:     2,
		ThroughputRPS: 4.0,
		Latencies: map[string]LatencyDist{
			"t_api_ack":    {Count: 18, P50Ms: 40, P95Ms: 60, P99Ms: 80},
			"t_first_ping": {Count: 18, P50Ms: 140, P95Ms: 280, P99Ms: 420},
		},
	}
	r3.LeakAudit = &LeakAudit{Passed: false, ResidualSandboxCRs: 1}

	agg := AggregateCycles("run", "ref", "fast-sandbox-overlaybd", []CycleRunInput{
		{Cycle: 1, Report: r1},
		{Cycle: 2, Report: r2},
		{Cycle: 3, Report: r3, Err: errors.New("cycle 3 failed")},
	})

	require.Equal(t, 3, agg.TotalCycles)
	require.Equal(t, 2, agg.PassedCycles)
	require.Equal(t, 1, agg.FailedCycles)
	require.InDelta(t, 2.0/3.0, agg.PassRate, 1e-6)
	require.Len(t, agg.Cycles, 3)
	require.True(t, agg.Cycles[0].Passed)
	require.True(t, agg.Cycles[1].Passed)
	require.False(t, agg.Cycles[2].Passed)

	sb, ok := agg.Suites["schedule-burst"]
	require.True(t, ok)
	require.Equal(t, 3, sb.CyclesRun)
	require.Equal(t, 2, sb.CyclesPassed)
	require.InDelta(t, 10.0, sb.MeanRPS, 1e-6)
	require.InDelta(t, 4.0, sb.MinRPS, 1e-6)
	require.InDelta(t, 16.0, sb.MaxRPS, 1e-6)
	require.NotNil(t, sb.TAPIAck)
	require.InDelta(t, 30.0, sb.TAPIAck.P50Ms, 1e-6)
	require.InDelta(t, 50.0, sb.TAPIAck.P95Ms, 1e-6)
	require.InDelta(t, 70.0, sb.TAPIAck.P99Ms, 1e-6)
	require.NotNil(t, sb.TFirstPing)
	require.InDelta(t, 120.0, sb.TFirstPing.P50Ms, 1e-6)
	require.InDelta(t, 240.0, sb.TFirstPing.P95Ms, 1e-6)
	require.InDelta(t, 360.0, sb.TFirstPing.P99Ms, 1e-6)

	outDir := t.TempDir()
	require.NoError(t, agg.WriteBundle(outDir))

	rawJSON, err := os.ReadFile(filepath.Join(outDir, "cycles-report.json"))
	require.NoError(t, err)
	var decoded CycleSummaryReport
	require.NoError(t, json.Unmarshal(rawJSON, &decoded))
	require.Equal(t, 3, decoded.TotalCycles)

	mdBytes, err := os.ReadFile(filepath.Join(outDir, "summary.md"))
	require.NoError(t, err)
	md := string(mdBytes)
	require.Contains(t, md, "Multi-Cycle Benchmark Summary")
	require.Contains(t, md, "2 / 3")
	require.Contains(t, md, "10.00 / 4.00 / 16.00")
	require.Contains(t, md, "Per-Cycle Breakdown")
	require.Contains(t, md, "run-cycle-3")
}
