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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

func TestScheduleBurstSuite_Run(t *testing.T) {
	backend := newMockBenchBackend(t)
	backend.pendingEveryN = 4

	prof := testOverlayBDProfile()
	prof.Suites.ScheduleBurst = &profile.ScheduleBurstSuiteSpec{
		Concurrencies:    []int{1, 4},
		AttemptsPerLevel: 5,
		ReadinessPollMs:  5,
	}

	snap := &mockMetricsSnapshotter{}
	s := NewScheduleBurstSuite(WithSuiteHooks(BenchHooks{
		MetricsSnapshotter: snap,
	}))
	require.Equal(t, "schedule-burst", s.Name())
	require.Equal(t, []string{profile.RuntimeFastSandboxOverlayBD}, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:      "test-run-burst",
		Profile:    prof,
		SDKConfig:  backend.sdkConfig(),
		WarmupRuns: 2,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "schedule-burst", res.Name)
	require.Equal(t, "passed", res.Status)

	// 2 levels x 5 attempts = 10 measured ops (2 warmups excluded).
	require.Equal(t, 10, res.TotalOps)
	require.Len(t, samples, 10)
	require.Equal(t, 10, res.SucceededOps)
	require.Equal(t, 0, res.FailedOps)
	require.Greater(t, res.PendingFallbackOps, 0)
	require.Greater(t, res.ThroughputRPS, 0.0)

	// Latencies populated for all 3 client black-box stages.
	require.Equal(t, res.SucceededOps, res.Latencies["t_api_ack"].Count)
	require.Equal(t, res.SucceededOps, res.Latencies["t_first_ping"].Count)
	require.Equal(t, res.SucceededOps, res.Latencies["t_first_exec"].Count)

	// Per-level breakdown and pending ratio present in Extra.
	levels, ok := res.Extra["levels"].([]ScheduleBurstLevelResult)
	require.True(t, ok)
	require.Len(t, levels, 2)
	require.Equal(t, 1, levels[0].Concurrency)
	require.Equal(t, 4, levels[1].Concurrency)
	require.Greater(t, res.Extra["create_pending_fallback_rate"].(float64), 0.0)

	// Tracing skipped: TraceID empty and Sampled false.
	for _, sm := range samples {
		require.Empty(t, sm.TraceID)
		require.False(t, sm.Sampled)
	}

	// Metrics snapshotter hooks invoked and all sandboxes cleaned up.
	require.True(t, snap.beforeCalled)
	require.True(t, snap.afterCalled)
	require.NotNil(t, res.OverlayBDMetrics)
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestScheduleBurstSuite_PartialFailureAndZeroOpsFail(t *testing.T) {
	t.Run("partial 429 failure fails suite", func(t *testing.T) {
		backend := newMockBenchBackend(t)
		backend.rate429EveryN = 5

		prof := testOverlayBDProfile()
		prof.Suites.ScheduleBurst = &profile.ScheduleBurstSuiteSpec{
			Concurrencies:    []int{1, 4},
			AttemptsPerLevel: 5,
			ReadinessPollMs:  5,
		}

		s := NewScheduleBurstSuite()
		res, _, err := s.Run(context.Background(), RunContext{
			RunID:     "test-run-burst-429",
			Profile:   prof,
			SDKConfig: backend.sdkConfig(),
		})
		require.NoError(t, err)
		require.Equal(t, "failed", res.Status)
		require.Greater(t, res.SucceededOps, 0)
		require.Greater(t, res.FailedOps, 0)
		require.Greater(t, res.Errors["http_429"], 0)
		require.Greater(t, res.Extra["client_429_retry_after_rate"].(float64), 0.0)
		require.Equal(t, 0, backend.activeSandboxCount())
	})

	t.Run("zero total ops fails suite", func(t *testing.T) {
		backend := newMockBenchBackend(t)
		prof := testOverlayBDProfile()
		prof.Suites.ScheduleBurst = &profile.ScheduleBurstSuiteSpec{
			Concurrencies:    []int{0},
			AttemptsPerLevel: 5,
			ReadinessPollMs:  5,
		}

		s := NewScheduleBurstSuite()
		res, _, err := s.Run(context.Background(), RunContext{
			RunID:     "test-run-burst-zero",
			Profile:   prof,
			SDKConfig: backend.sdkConfig(),
		})
		require.NoError(t, err)
		require.Equal(t, "failed", res.Status)
		require.Equal(t, 0, res.TotalOps)
	})
}
