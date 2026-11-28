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
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

type fakeTimeoutNetErr struct{}

func (fakeTimeoutNetErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutNetErr) Timeout() bool   { return true }
func (fakeTimeoutNetErr) Temporary() bool { return true }

var _ net.Error = fakeTimeoutNetErr{}

func TestScaleErrorCodeAndStructuredErrorBreakdown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"exit 6 dns", &CommandExecError{ExitCode: 6, HasExit: true, Stderr: "could not resolve host"}, "exit_6_dns"},
		{"exit 7 conn refused", &CommandExecError{ExitCode: 7, HasExit: true, Stderr: "failed to connect"}, "exit_7_conn_refused"},
		{"exit 28 timeout", &CommandExecError{ExitCode: 28, HasExit: true, Stderr: "operation timed out"}, "exit_28_timeout"},
		{"exit 127 no tool", &CommandExecError{ExitCode: 127, HasExit: true, Stderr: "no http probe tool"}, "exit_127_no_tool"},
		{"exit 42 generic", &CommandExecError{ExitCode: 42, HasExit: true}, "exit_42"},
		{"execd error", &CommandExecError{EName: "ExecError", EValue: "killed"}, "execd_error"},
		{"net timeout", fakeTimeoutNetErr{}, "net_timeout"},
		{"conn reset", syscall.ECONNRESET, "conn_reset"},
		{"context deadline", context.DeadlineExceeded, "timeout"},
		{"generic", errors.New("boom"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, scaleErrorCode(tc.err))
		})
	}

	rec := newScaleRecorder()
	errDNS := &CommandExecError{ExitCode: 6, HasExit: true, Stderr: "curl: (6) Could not resolve host: example.com\n"}
	rec.recordOp(OperationSample{
		Operation:  "soak_egress_0.50x",
		SandboxID:  "sbx-1",
		Status:     "error",
		ErrorCode:  scaleErrorCode(errDNS),
		StderrHead: scaleErrorStderrHead(errDNS, 200),
	})
	require.Equal(t, 1, rec.errors["exit_6_dns"])
	require.Equal(t, 1, rec.errorsByOp["soak_egress_0.50x"]["exit_6_dns"])
	require.Len(t, rec.errorSamples, 1)
	require.Equal(t, "soak_egress_0.50x", rec.errorSamples[0].Operation)
	require.Equal(t, "exit_6_dns", rec.errorSamples[0].Code)
	require.Contains(t, rec.errorSamples[0].StderrHead, "Could not resolve host")
}

func TestHighDensitySoak_RequireNodeMemoryFailFastAndProfileOverride(t *testing.T) {
	_, ts := newMockSoakServer(0)
	defer ts.Close()

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack:   profile.StackSpec{Template: profile.TemplateSpec{MemoryMiB: 1024}},
		Suites: profile.SuitesSpec{
			HighDensitySoak: &profile.HighDensitySoakSuiteSpec{
				OversellTiers:          []float64{0.5},
				DurationSecondsPerTier: 1,
				MeasureRecovery:        false,
			},
		},
	}

	s := NewHighDensitySoakSuite(
		WithSoakEgressProbeURL("none"),
		WithSoakPollInterval(2*time.Millisecond),
		WithSoakTickInterval(100*time.Millisecond),
	)

	// 1. RequireNodeMemory=true with 0 detected memory must fail-fast.
	ctx := WithBenchHooks(context.Background(), BenchHooks{RequireNodeMemory: true})
	_, _, err := s.Run(ctx, RunContext{
		RunID:     "test-failfast",
		Profile:   prof,
		SDKConfig: opensandbox.ConnectionConfig{Domain: ts.URL, HTTPClient: ts.Client()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nodeMemoryMiB")

	// 2. Profile override nodeMemoryMiB succeeds and records node_memory_source=profile_override + invokes ActiveSandboxReporter.
	prof.Suites.HighDensitySoak.NodeMemoryMiB = 4096
	var reportedCounts []int
	var repMu sync.Mutex
	ctx2 := WithBenchHooks(context.Background(), BenchHooks{
		RequireNodeMemory: true,
		ActiveSandboxReporter: func(n int) {
			repMu.Lock()
			reportedCounts = append(reportedCounts, n)
			repMu.Unlock()
		},
	})
	res, _, err := s.Run(ctx2, RunContext{
		RunID:     "test-override",
		Profile:   prof,
		SDKConfig: opensandbox.ConnectionConfig{Domain: ts.URL, HTTPClient: ts.Client()},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, "profile_override", res.Extra["node_memory_source"])
	require.Equal(t, 4096, res.Extra["node_memory_mib"])
	require.Contains(t, reportedCounts, 2)
	require.Equal(t, 0, reportedCounts[len(reportedCounts)-1])
}

func TestDensityRamp_StepTelemetryAndActiveSandboxReporter(t *testing.T) {
	mockSrv := newMockDensityServer(0, nil)
	ts := mockSrv.server
	defer ts.Close()

	var snapCall atomic.Int32
	stepSampler := StepTelemetrySamplerFunc(func(_ context.Context) (*StepTelemetrySnapshot, error) {
		n := snapCall.Add(1)
		if n == 1 {
			// Step 0 baseline.
			return &StepTelemetrySnapshot{
				MemTotalMiB:     64000,
				MemAvailableMiB: 60000,
				SlabMiB:         1000,
				SUnreclaimMiB:   400,
			}, nil
		}
		// Step 1 with 4 live sandboxes: 400 MiB MemAvail drop -> 100 MiB/sbx, 40 MiB Slab delta -> 10 MiB/sbx.
		return &StepTelemetrySnapshot{
			MemTotalMiB:     64000,
			MemAvailableMiB: 59600,
			SlabMiB:         1040,
			SUnreclaimMiB:   420,
			FCPIDCount:      4,
			FCPssAvgMiB:     42.5,
			FCRssAvgMiB:     64.0,
			FastletRSSMiB:   110.0,
			EgressRSSMiB:    45.0,
			UblkdRSSMiB:     180.0,
			ProxyRSSMiB:     28.0,
			CPUUtilPct:      24.5,
			Load1m:          2.4,
		}, nil
	})

	var reported []int
	var repMu sync.Mutex
	ctx := WithBenchHooks(context.Background(), BenchHooks{
		StepTelemetrySampler: stepSampler,
		ActiveSandboxReporter: func(n int) {
			repMu.Lock()
			reported = append(reported, n)
			repMu.Unlock()
		},
	})

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Suites: profile.SuitesSpec{
			DensityRamp: &profile.DensityRampSuiteSpec{
				Steps:                  []int{4},
				BatchCreateConcurrency: 4,
				MaxBatchFailureRate:    0.1,
				PingSampleSize:         2,
				ReadinessPollMs:        2,
			},
		},
	}

	s := NewDensityRampSuite(WithDensityRampPollInterval(2 * time.Millisecond))
	res, _, err := s.Run(ctx, RunContext{
		RunID:     "test-density-telemetry",
		Profile:   prof,
		SDKConfig: opensandbox.ConnectionConfig{Domain: ts.URL, HTTPClient: ts.Client()},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Contains(t, res.Latencies, "t_endpoint_resolve")

	steps, ok := res.Extra["steps"].([]DensityRampStepResult)
	require.True(t, ok)
	require.Len(t, steps, 1)
	require.InDelta(t, 100.0, steps[0].DeltaMemAvailableMiBPerSbx, 0.01)
	require.InDelta(t, 42.5, steps[0].FCPssMiBPerSbx, 0.01)
	require.InDelta(t, 64.0, steps[0].FCRssMiBPerSbx, 0.01)
	require.InDelta(t, 10.0, steps[0].DeltaSlabMiBPerSbx, 0.01)
	require.InDelta(t, 5.0, steps[0].DeltaSUnreclaimMiBPerSbx, 0.01)
	require.InDelta(t, 110.0, steps[0].FastletRSSMiB, 0.01)
	require.InDelta(t, 180.0, steps[0].UblkdRSSMiB, 0.01)
	require.InDelta(t, 24.5, steps[0].HostCPUUtilPct, 0.01)
	require.InDelta(t, 2.4, steps[0].HostLoad1m, 0.01)

	require.Contains(t, reported, 4)
	require.Equal(t, 0, reported[len(reported)-1])
}
