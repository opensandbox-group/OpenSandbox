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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

func TestPauseResumeSuite_MatrixAndChainSquash(t *testing.T) {
	backend := newMockBenchBackend(t)
	// Simulate a squash spike on round 25 (crossing defaultMaxRootfsLayers=24).
	backend.pauseDelayFn = func(_ *mockSandbox, round int) time.Duration {
		if round == 25 {
			return 120 * time.Millisecond
		}
		return 1 * time.Millisecond
	}

	evictor := &mockCacheEvictor{}
	isolator := &mockNodeIsolator{backend: backend}

	prof := testOverlayBDProfile()
	prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
		Modes:                        []string{"warm", "cache-evicted", "cross-node"},
		DirtyUpperMiB:                []int{0, 16},
		ChainSquashRounds:            26,
		ChainSquashTemplateMemoryMiB: 512,
	}

	ctx := WithBenchHooks(context.Background(), BenchHooks{
		CacheEvictor: evictor,
		NodeIsolator: isolator,
	})

	s := NewPauseResumeSuite()
	require.Equal(t, "pause-resume", s.Name())
	require.Equal(t, []string{profile.RuntimeFastSandboxOverlayBD}, s.SupportedRuntimes())

	res, samples, err := s.Run(ctx, RunContext{
		RunID:     "test-run-pr",
		Profile:   prof,
		SDKConfig: backend.sdkConfig(),
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "pause-resume", res.Name)
	require.Equal(t, "passed", res.Status)

	// 3 modes x 2 dirty sizes = 6 matrix cases + 26 chain rounds = 32 ops.
	require.Equal(t, 32, res.TotalOps)
	require.Equal(t, 32, res.SucceededOps)
	require.Equal(t, 0, res.FailedOps)
	require.NotEmpty(t, samples)

	for _, key := range []string{"t_api_ack", "t_first_ping", "t_first_exec", "t_pause_api_ack", "t_pause_total"} {
		require.Equal(t, 32, res.Latencies[key].Count, "latency dist %s", key)
	}

	matrix, ok := res.Extra["matrix"].([]PauseResumeCaseResult)
	require.True(t, ok)
	require.Len(t, matrix, 6)
	for _, c := range matrix {
		require.Equal(t, "ok", c.Status)
		require.True(t, c.DiskMarkerVerified)
		require.True(t, c.MemProcessVerified)
		switch c.Mode {
		case "cache-evicted":
			require.True(t, c.CacheEvicted)
		case "cross-node":
			require.True(t, c.CrossNodeVerified)
			require.NotEqual(t, c.BeforeNode, c.AfterNode)
		}
	}

	// Evictor called once per cache-evicted dirty size (2); isolator called & restored for cross-node (2).
	require.Len(t, evictor.nodes, 2)
	require.Len(t, isolator.isolated, 2)
	require.Len(t, isolator.restored, 2)

	// Chain squash summary exposes the round 25 spike.
	chain, ok := res.Extra["chain_squash"].(*ChainSquashSummary)
	require.True(t, ok)
	require.Equal(t, 26, chain.RoundsConfigured)
	require.Equal(t, 26, chain.RoundsCompleted)
	require.Equal(t, 24, chain.DefaultMaxRootfsLayers)
	require.Equal(t, 25, chain.MaxPauseRound)
	require.Greater(t, chain.SquashCliffRatio, 2.0)

	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestPauseResumeSuite_CrossNodePlacementFailure(t *testing.T) {
	backend := newMockBenchBackend(t)
	backend.ignoreIsolation = true // force resumed sandbox to stay on node-a

	prof := testOverlayBDProfile()
	prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
		Modes:             []string{"cross-node"},
		DirtyUpperMiB:     []int{0},
		ChainSquashRounds: 0,
	}

	s := NewPauseResumeSuite()
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "test-run-pr-fail",
		Profile:   prof,
		SDKConfig: backend.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.FailedOps)
	require.Equal(t, 0, res.SucceededOps)

	matrix := res.Extra["matrix"].([]PauseResumeCaseResult)
	require.Len(t, matrix, 1)
	require.Equal(t, "error", matrix[0].Status)
	require.Contains(t, matrix[0].Error, "cross-node placement assertion failed")
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestPauseResumeSuite_MemContinuityFailure(t *testing.T) {
	backend := newMockBenchBackend(t)
	backend.failMemVerify = true

	prof := testOverlayBDProfile()
	prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
		Modes:             []string{"warm"},
		DirtyUpperMiB:     []int{0},
		ChainSquashRounds: 0,
	}

	s := NewPauseResumeSuite()
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "test-run-pr-mem-fail",
		Profile:   prof,
		SDKConfig: backend.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.FailedOps)
	matrix := res.Extra["matrix"].([]PauseResumeCaseResult)
	require.True(t, matrix[0].DiskMarkerVerified)
	require.False(t, matrix[0].MemProcessVerified)
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestPauseResumeSuite_PostResumeTransient503Retry(t *testing.T) {
	backend := newMockBenchBackend(t)
	// Inject one transient 503 on the first post-resume verify exec so attempt 2 succeeds.
	backend.verifyExec503Remaining.Store(1)

	prof := testOverlayBDProfile()
	prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
		Modes:             []string{"warm"},
		DirtyUpperMiB:     []int{0},
		ChainSquashRounds: 0,
	}

	s := NewPauseResumeSuite()
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "test-run-pr-503-retry",
		Profile:   prof,
		SDKConfig: backend.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, 1, res.SucceededOps)
	require.Equal(t, 0, res.FailedOps)
	require.Equal(t, int32(2), backend.verifyExecAttempts.Load())

	matrix := res.Extra["matrix"].([]PauseResumeCaseResult)
	require.Len(t, matrix, 1)
	require.Equal(t, "ok", matrix[0].Status)
	require.True(t, matrix[0].DiskMarkerVerified)
	require.True(t, matrix[0].MemProcessVerified)
	require.GreaterOrEqual(t, matrix[0].ResumeFirstExecMs, matrix[0].ResumeFirstPingMs)
	require.GreaterOrEqual(t, matrix[0].ResumeFirstExecMs, float64(postResumeExecRetryBackoff.Milliseconds()))
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestPauseResumeSuite_CacheEvictedRequiresEvictor(t *testing.T) {
	backend := newMockBenchBackend(t)

	prof := testOverlayBDProfile()
	prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
		Modes:             []string{"cache-evicted"},
		DirtyUpperMiB:     []int{0},
		ChainSquashRounds: 0,
	}

	s := NewPauseResumeSuite()
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "test-run-pr-no-evictor",
		Profile:   prof,
		SDKConfig: backend.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.FailedOps)
	require.Equal(t, 0, res.SucceededOps)
	matrix := res.Extra["matrix"].([]PauseResumeCaseResult)
	require.Len(t, matrix, 1)
	require.Equal(t, "error", matrix[0].Status)
	require.False(t, matrix[0].CacheEvicted)
	require.Contains(t, matrix[0].Error, "CacheEvictor")
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestPauseResumeSuite_ChainSquashTruncatedAndRoundMarkerFailure(t *testing.T) {
	t.Run("truncated chain squash fails suite", func(t *testing.T) {
		backend := newMockBenchBackend(t)
		ctx, cancel := context.WithCancel(context.Background())
		backend.pauseDelayFn = func(_ *mockSandbox, round int) time.Duration {
			if round == 2 {
				cancel()
			}
			return 1 * time.Millisecond
		}

		prof := testOverlayBDProfile()
		prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
			Modes:             []string{"warm"},
			DirtyUpperMiB:     []int{0},
			ChainSquashRounds: 5,
		}

		s := NewPauseResumeSuite()
		res, _, err := s.Run(ctx, RunContext{
			RunID:     "test-run-pr-trunc",
			Profile:   prof,
			SDKConfig: backend.sdkConfig(),
		})
		require.NoError(t, err)
		require.Equal(t, "failed", res.Status)
		require.Greater(t, res.FailedOps, 0)
		chain := res.Extra["chain_squash"].(*ChainSquashSummary)
		require.Less(t, chain.RoundsCompleted, 5)
		require.Equal(t, 0, backend.activeSandboxCount())
	})

	t.Run("round marker corruption fails chain squash", func(t *testing.T) {
		backend := newMockBenchBackend(t)
		backend.failRoundMarker = true

		prof := testOverlayBDProfile()
		prof.Suites.PauseResume = &profile.PauseResumeSuiteSpec{
			Modes:             []string{"warm"},
			DirtyUpperMiB:     []int{0},
			ChainSquashRounds: 2,
		}

		s := NewPauseResumeSuite()
		res, _, err := s.Run(context.Background(), RunContext{
			RunID:     "test-run-pr-round-corrupt",
			Profile:   prof,
			SDKConfig: backend.sdkConfig(),
		})
		require.NoError(t, err)
		require.Equal(t, "failed", res.Status)
		require.Equal(t, 1, res.FailedOps)
		chain := res.Extra["chain_squash"].(*ChainSquashSummary)
		require.Equal(t, 0, chain.RoundsCompleted)
		require.Contains(t, chain.Rounds[0].Error, "round 1 marker mismatch")
		require.Equal(t, 0, backend.activeSandboxCount())
	})
}
