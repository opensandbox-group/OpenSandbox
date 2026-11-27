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

package suite

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

func TestNoisyNeighborSuite_QuantifiesDegradationAndCleansUp(t *testing.T) {
	var stressActive atomic.Bool
	var aggressorStarts atomic.Int32

	srv := newMockMicrobenchServer(func(_, command string, background bool) (string, int) {
		if background {
			stressActive.Store(true)
			aggressorStarts.Add(1)
			return "", 0
		}
		// Simulate deterministic baseline vs noisy victim latency.
		if stressActive.Load() {
			time.Sleep(200 * time.Millisecond)
		}
		return "ok", 0
	})
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			NoisyNeighbor: &profile.NoisyNeighborSuiteSpec{
				FleetSize:             5,
				AggressorRatio:        0.20,
				ProbeRounds:           2,
				StressModes:           []string{"cpu", "disk", "network"},
				StressDurationSeconds: 5,
				ReadinessPollMs:       1,
			},
		},
	}

	s := NewNoisyNeighborSuite(WithNoisyNeighborStressWarmup(time.Millisecond))
	require.Equal(t, "noisy-neighbor", s.Name())
	require.Nil(t, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:     "noisy-test-1",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.NotEmpty(t, samples)

	// 5 sandboxes with 0.20 ratio -> 1 aggressor, 4 victims.
	require.Equal(t, 1, res.Extra["aggressor_count"])
	require.Equal(t, 4, res.Extra["victim_count"])
	require.Equal(t, int32(1), aggressorStarts.Load())

	// 2 rounds x 4 victims = 8 baseline probes and 8 noisy probes.
	require.Equal(t, 8, res.Latencies["victim_baseline_exec"].Count)
	require.Equal(t, 8, res.Latencies["victim_noisy_exec"].Count)

	// Noisy phase should exhibit clear latency increase and exact degradation ratio formulas.
	baseDist := res.Latencies["victim_baseline_exec"]
	noisyDist := res.Latencies["victim_noisy_exec"]
	p99Ratio := res.Extra["p99_degradation_ratio"].(float64)
	p50Ratio := res.Extra["p50_degradation_ratio"].(float64)
	require.InDelta(t, noisyDist.P99Ms/baseDist.P99Ms, p99Ratio, 1e-6)
	require.InDelta(t, noisyDist.P50Ms/baseDist.P50Ms, p50Ratio, 1e-6)
	require.GreaterOrEqual(t, noisyDist.P50Ms, 190.0)
	require.Greater(t, p50Ratio, 1.5)
	require.InDelta(t, 0.0, res.Extra["failure_rate_delta"].(float64), 0.001)

	// All 5 sandboxes must be cleaned up.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs)
	require.Len(t, srv.deletedIDs, 5)
}

func TestNoisyNeighborSuite_MinOneAggressorAndStressCommand(t *testing.T) {
	cmd := buildAggressorStressCommand([]string{"cpu", "disk", "network"}, 15, "http://server/health")
	require.Contains(t, cmd, "dd if=/dev/zero")
	require.Contains(t, cmd, "urllib.request.urlopen")
	require.True(t, strings.HasSuffix(cmd, "wait'"))

	srv := newMockMicrobenchServer(func(_, _ string, _ bool) (string, int) {
		return "ok", 0
	})
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			NoisyNeighbor: &profile.NoisyNeighborSuiteSpec{
				FleetSize:             3,
				AggressorRatio:        0.05, // rounds to 0, must clamp to 1 aggressor and 2 victims
				ProbeRounds:           2,
				StressModes:           []string{"cpu"},
				StressDurationSeconds: 2,
				ReadinessPollMs:       1,
			},
		},
	}

	s := NewNoisyNeighborSuite(WithNoisyNeighborStressWarmup(0))
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "noisy-test-clamp",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, 1, res.Extra["aggressor_count"])
	require.Equal(t, 2, res.Extra["victim_count"])

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs)
	require.Len(t, srv.deletedIDs, 3)
}
