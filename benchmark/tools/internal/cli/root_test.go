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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/collector"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/suite"
)

type stubWorkerSuite struct {
	name   string
	result *report.SuiteResult
	runErr error
}

func (s *stubWorkerSuite) Name() string                { return s.name }
func (s *stubWorkerSuite) Description() string         { return "stub suite" }
func (s *stubWorkerSuite) SupportedRuntimes() []string { return []string{profile.RuntimeFastSandboxOverlayBD} }
func (s *stubWorkerSuite) Run(_ context.Context, _ suite.RunContext) (*report.SuiteResult, []suite.OperationSample, error) {
	return s.result, nil, s.runErr
}

type multiCycleStubSuite struct {
	name    string
	calls   int
	results []*report.SuiteResult
}

func (s *multiCycleStubSuite) Name() string                { return s.name }
func (s *multiCycleStubSuite) Description() string         { return "multi-cycle stub" }
func (s *multiCycleStubSuite) SupportedRuntimes() []string { return nil }
func (s *multiCycleStubSuite) Run(_ context.Context, _ suite.RunContext) (*report.SuiteResult, []suite.OperationSample, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	return s.results[idx], nil, nil
}

func TestCLIWorkerRunMultiCycleWritesPerCycleAndAggregateBundle(t *testing.T) {
	outDir := t.TempDir()
	stub := &multiCycleStubSuite{
		name: "schedule-burst",
		results: []*report.SuiteResult{
			{
				Name:          "schedule-burst",
				Status:        "passed",
				TotalOps:      10,
				SucceededOps:  10,
				ThroughputRPS: 12.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 10, P50Ms: 10, P95Ms: 20, P99Ms: 30},
					"t_first_ping": {Count: 10, P50Ms: 100, P95Ms: 200, P99Ms: 300},
				},
			},
			{
				Name:          "schedule-burst",
				Status:        "passed",
				TotalOps:      10,
				SucceededOps:  10,
				ThroughputRPS: 18.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 10, P50Ms: 20, P95Ms: 30, P99Ms: 40},
					"t_first_ping": {Count: 10, P50Ms: 120, P95Ms: 220, P99Ms: 320},
				},
			},
			{
				Name:          "schedule-burst",
				Status:        "failed",
				TotalOps:      10,
				SucceededOps:  8,
				FailedOps:     2,
				ThroughputRPS: 6.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 8, P50Ms: 30, P95Ms: 40, P99Ms: 50},
					"t_first_ping": {Count: 8, P50Ms: 140, P95Ms: 240, P99Ms: 340},
				},
			},
		},
	}

	reg := suite.NewRegistry()
	reg.MustRegister(stub)

	app := NewDefaultApp()
	app.Suites = reg
	app.NewCollectorClients = func() (*collector.Clients, error) {
		return nil, errors.New("sdk-only")
	}

	cmd := app.RootCmd()
	cmd.SetArgs([]string{
		"worker", "run",
		"--suite", "schedule-burst",
		"--output", outDir,
		"--run-id", "multi-run",
		"--cycles", "3",
		"--allow-degraded-telemetry",
	})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 of 3 cycles failed")
	require.Equal(t, 3, stub.calls)

	for _, c := range []string{"cycle-1", "cycle-2", "cycle-3"} {
		_, statErr := os.Stat(filepath.Join(outDir, c, "report.json"))
		require.NoError(t, statErr)
	}

	rawAgg, readErr := os.ReadFile(filepath.Join(outDir, "cycles-report.json"))
	require.NoError(t, readErr)
	var agg report.CycleSummaryReport
	require.NoError(t, json.Unmarshal(rawAgg, &agg))
	require.Equal(t, 3, agg.TotalCycles)
	require.Equal(t, 2, agg.PassedCycles)
	require.InDelta(t, 12.0, agg.Suites["schedule-burst"].MeanRPS, 1e-6)
}

func TestCLIPlotSubcommand(t *testing.T) {
	dir := t.TempDir()
	rep := report.New(report.Provenance{RunID: "cli-plot-test", Runtime: "fast-sandbox-overlaybd"})
	rep.Suites["schedule-burst"] = report.SuiteResult{
		Name:         "schedule-burst",
		Status:       "passed",
		TotalOps:     10,
		SucceededOps: 10,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":    {Count: 10, MinMs: 50, P50Ms: 100, P90Ms: 140, P95Ms: 150, P99Ms: 180, MaxMs: 190},
			"t_first_ping": {Count: 10, MinMs: 60, P50Ms: 110, P90Ms: 150, P95Ms: 160, P99Ms: 190, MaxMs: 200},
		},
	}
	require.NoError(t, rep.WriteBundle(dir))

	outHTML := filepath.Join(dir, "custom.html")
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"plot", filepath.Join(dir, "report.json"), "--output", outHTML, "--title", "CLI Plot Smoke"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), outHTML)

	raw, err := os.ReadFile(outHTML)
	require.NoError(t, err)
	require.Contains(t, string(raw), "CLI Plot Smoke")
	require.Contains(t, string(raw), "t_api_ack")
	require.Contains(t, string(raw), "t_first_ping")
	require.NotContains(t, string(raw), "t_first_exec")
}

func TestCLIWorkerTelemetryPreflightFailFastAndDegradedFlag(t *testing.T) {
	stub := &stubWorkerSuite{
		name: "schedule-burst",
		result: &report.SuiteResult{
			Name:            "schedule-burst",
			Status:          "passed",
			StartedAt:       time.Now().UTC(),
			DurationSeconds: 1.0,
			TotalOps:        5,
			SucceededOps:    5,
			Latencies: map[string]report.LatencyDist{
				"t_api_ack":          {Count: 5, P50Ms: 50, P99Ms: 90},
				"t_endpoint_resolve": {Count: 5, P50Ms: 70, P99Ms: 110},
				"t_first_ping":       {Count: 5, P50Ms: 150, P99Ms: 220},
			},
		},
	}
	reg := suite.NewRegistry()
	reg.MustRegister(stub)

	badCS := fake.NewSimpleClientset()
	badCS.PrependReactor("list", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("403 forbidden")
	})

	app := NewDefaultApp()
	app.Suites = reg
	app.NewCollectorClients = func() (*collector.Clients, error) {
		return &collector.Clients{Clientset: badCS}, nil
	}

	// Default mode fails fast when telemetry preflight detects missing RBAC/exec/dynamic capabilities.
	cmd := app.RootCmd()
	cmd.SetArgs([]string{
		"worker", "run",
		"--suite", "schedule-burst",
		"--output", t.TempDir(),
	})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "telemetry preflight failed")

	// Explicit --allow-degraded-telemetry permits execution and stamps telemetry_status=degraded.
	outDir := t.TempDir()
	cmd2 := app.RootCmd()
	cmd2.SetArgs([]string{
		"worker", "run",
		"--suite", "schedule-burst",
		"--output", outDir,
		"--allow-degraded-telemetry",
	})
	require.NoError(t, cmd2.Execute())

	rep, loadErr := report.Load(filepath.Join(outDir, "report.json"))
	require.NoError(t, loadErr)
	require.Equal(t, collector.TelemetryStatusDegraded, rep.TelemetryStatus)
	require.NotEmpty(t, rep.TelemetryMissing)
	require.NotEmpty(t, rep.Suites["schedule-burst"].StageAttribution)
	require.NotNil(t, rep.LeakAudit)
	require.False(t, rep.LeakAudit.Passed, "degraded telemetry must never produce LeakAudit PASS")
	require.True(t, rep.LeakAudit.Incomplete)
	require.NotEmpty(t, rep.LeakAudit.Checks)
}
