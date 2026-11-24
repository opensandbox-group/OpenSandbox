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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComputeLatencyDistNearestRank(t *testing.T) {
	samples := make([]float64, 100)
	for i := 0; i < 100; i++ {
		samples[i] = float64(i + 1)
	}
	d := ComputeLatencyDist(samples)
	require.Equal(t, 100, d.Count)
	require.Equal(t, 1.0, d.MinMs)
	require.Equal(t, 50.0, d.P50Ms)
	require.Equal(t, 90.0, d.P90Ms)
	require.Equal(t, 95.0, d.P95Ms)
	require.Equal(t, 99.0, d.P99Ms)
	require.Equal(t, 100.0, d.MaxMs)
	require.Equal(t, 50.5, d.MeanMs)
}

func TestCompareDetectsRegressionsAndRoundTrip(t *testing.T) {
	base := New(Provenance{
		RunID:             "run-base",
		ProfileName:       "fast-sandbox-overlaybd-ref",
		Runtime:           "fast-sandbox-overlaybd",
		OpenSandboxTag:    "v0.9.0",
		OpenSandboxCommit: "aaaaaaa",
		FastSandboxCommit: "bbbbbbb",
	})
	base.Suites["schedule-burst"] = SuiteResult{
		Name:          "schedule-burst",
		Status:        "passed",
		ThroughputRPS: 20.0,
		TotalOps:      100,
		SucceededOps:  100,
		Latencies: map[string]LatencyDist{
			"t_first_ping": {Count: 100, P50Ms: 80, P95Ms: 120, P99Ms: 150},
		},
	}
	base.LeakAudit = &LeakAudit{Passed: true}

	cand := New(Provenance{
		RunID:             "run-cand",
		ProfileName:       "fast-sandbox-overlaybd-ref",
		Runtime:           "fast-sandbox-overlaybd",
		OpenSandboxTag:    "v0.10.0",
		OpenSandboxCommit: "ccccccc",
		FastSandboxCommit: "ddddddd",
	})
	cand.Suites["schedule-burst"] = SuiteResult{
		Name:          "schedule-burst",
		Status:        "passed",
		ThroughputRPS: 16.0, // -20% drop (> 10% threshold)
		TotalOps:      100,
		SucceededOps:  100,
		Latencies: map[string]LatencyDist{
			"t_first_ping": {Count: 100, P50Ms: 82, P95Ms: 150, P99Ms: 200}, // +25% / +33%
		},
	}
	cand.LeakAudit = &LeakAudit{Passed: false, OrphanedUblkDevices: 1}

	dir := t.TempDir()
	require.NoError(t, base.WriteBundle(filepath.Join(dir, "base")))
	require.NoError(t, cand.WriteBundle(filepath.Join(dir, "cand")))

	loadedBase, err := Load(filepath.Join(dir, "base", "report.json"))
	require.NoError(t, err)
	loadedCand, err := Load(filepath.Join(dir, "cand", "report.json"))
	require.NoError(t, err)

	res := Compare(loadedBase, loadedCand, DefaultCompareThresholds())
	require.True(t, res.HasRegression)
	require.GreaterOrEqual(t, len(res.Regressions), 3)
	require.Contains(t, res.MarkdownSummary, "REGRESSION")
}
