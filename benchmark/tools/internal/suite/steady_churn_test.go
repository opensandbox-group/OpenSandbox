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

func TestSteadyChurnSuite_Run(t *testing.T) {
	backend := newMockBenchBackend(t)

	prof := testOverlayBDProfile()
	prof.Suites.SteadyChurn = &profile.SteadyChurnSuiteSpec{
		TargetRPS:       15,
		DurationSeconds: 1,
		ReadinessPollMs: 5,
	}

	s := NewSteadyChurnSuite()
	require.Equal(t, "steady-churn", s.Name())
	require.Equal(t, []string{profile.RuntimeFastSandboxOverlayBD}, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:      "test-run-churn",
		Profile:    prof,
		SDKConfig:  backend.sdkConfig(),
		WarmupRuns: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "steady-churn", res.Name)
	require.Equal(t, "passed", res.Status)
	require.GreaterOrEqual(t, res.TotalOps, 5)
	require.Equal(t, res.TotalOps, res.SucceededOps)
	require.Equal(t, 0, res.FailedOps)
	require.Len(t, samples, res.TotalOps)
	require.Greater(t, res.ThroughputRPS, 0.0)

	for _, key := range []string{"t_api_ack", "t_first_ping", "t_first_exec", "t_delete", "t_cycle_total"} {
		require.Equal(t, res.SucceededOps, res.Latencies[key].Count, "latency dist %s", key)
	}
	require.Equal(t, 15.0, res.Extra["target_rps"])
	require.Greater(t, res.Extra["achieved_rps"].(float64), 0.0)

	for _, sm := range samples {
		require.Equal(t, "churn_cycle", sm.Operation)
		require.Equal(t, "ok", sm.Status)
		require.Empty(t, sm.TraceID)
		require.False(t, sm.Sampled)
	}

	require.Equal(t, 0, backend.activeSandboxCount())
}
