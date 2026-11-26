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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

type mockSoakServer struct {
	mu           sync.Mutex
	serverURL    string
	seq          atomic.Int64
	sandboxes    map[string]opensandbox.SandboxState
	execCount    atomic.Int64
	egressCount  atomic.Int64
	ingressCount atomic.Int64
	denyAbove    int // if > 0, return 429 when active >= denyAbove
}

func newMockSoakServer(denyAbove int) (*mockSoakServer, *httptest.Server) {
	m := &mockSoakServer{
		sandboxes: make(map[string]opensandbox.SandboxState),
		denyAbove: denyAbove,
	}
	ts := httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.serverURL = ts.URL
	return m, ts
}

func (m *mockSoakServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/sandboxes":
		m.mu.Lock()
		if m.denyAbove > 0 && len(m.sandboxes) >= m.denyAbove {
			m.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"code":    "ADMISSION_DENIED",
				"message": "memory budget exceeded",
			})
			return
		}
		id := fmt.Sprintf("soak-sbx-%d", m.seq.Add(1))
		m.sandboxes[id] = opensandbox.StateRunning
		m.mu.Unlock()
		writeJSON(w, http.StatusAccepted, opensandbox.SandboxInfo{
			ID:        id,
			Status:    opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			CreatedAt: time.Now().UTC(),
		})

	case r.Method == http.MethodGet && path == "/v1/sandboxes":
		m.mu.Lock()
		items := make([]opensandbox.SandboxInfo, 0, len(m.sandboxes))
		for id, st := range m.sandboxes {
			items = append(items, opensandbox.SandboxInfo{
				ID:     id,
				Status: opensandbox.SandboxStatus{State: st},
			})
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, opensandbox.ListSandboxesResponse{
			Items: items,
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/")
		sbxID := parts[0]
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: fmt.Sprintf("%s/execd/%s", m.serverURL, sbxID),
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		delete(m.sandboxes, sbxID)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/ping"):
		m.ingressCount.Add(1)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/command"):
		var req opensandbox.RunCommandRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Command, "curl") {
			m.egressCount.Add(1)
		} else {
			m.execCount.Add(1)
		}
		writeExecdSSE(w, "soak-ok")

	default:
		http.NotFound(w, r)
	}
}

func TestHighDensitySoakSuite_TiersSamplerAndRecovery(t *testing.T) {
	mock, ts := newMockSoakServer(0)
	defer ts.Close()

	var samplerCalls atomic.Int32
	sampler := SoakLoadSamplerFunc(func(_ context.Context) (*SoakLoadSample, error) {
		n := samplerCalls.Add(1)
		deleting := 0
		if n == 1 {
			deleting = 0
		}
		return &SoakLoadSample{
			ResidentMiBPerSandbox: 128.5,
			CPUPressureSomeAvg60:  3.2,
			MemPressureFullAvg60:  0.4,
			MemAvailableMiB:       2048.0,
			OOMKills:              0,
			DeletingSlots:         deleting,
			StateRootFreeGiB:      45.0,
			LoadProfiles: &report.LoadProfiles{
				Host: &report.ComponentLoadSummary{
					CPUCores: &report.StatSummary{Avg: 2.1, P95: 3.4, Max: 4.0},
				},
			},
		}, nil
	})

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack: profile.StackSpec{
			Template: profile.TemplateSpec{
				BaseImage: "opensandbox/test:latest",
				VCPUs:     1,
				MemoryMiB: 1024,
				DiskGiB:   2,
			},
		},
		Suites: profile.SuitesSpec{
			HighDensitySoak: &profile.HighDensitySoakSuiteSpec{
				OversellTiers:          []float64{0.5, 1.0},
				DurationSecondsPerTier: 1,
				MeasureRecovery:        true,
				MinStateRootFreeGiB:    10,
				SamplerIntervalSeconds: 1,
			},
		},
	}

	s := NewHighDensitySoakSuite(
		WithSoakLoadSampler(sampler),
		WithSoakNodeMemoryMiB(4096), // 0.5 -> 2 sandboxes, 1.0 -> 4 sandboxes
		WithSoakEgressProbeURL("https://probe.local/healthz"),
		WithSoakPollInterval(2*time.Millisecond),
		WithSoakTickInterval(100*time.Millisecond),
	)
	require.Equal(t, "high-density-soak", s.Name())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:   "test-soak-1",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Zero(t, res.FailedOps)
	require.Greater(t, len(samples), 0)

	// Verify all 4 workload types ran.
	require.Greater(t, mock.execCount.Load(), int64(0))
	require.Greater(t, mock.egressCount.Load(), int64(0))
	require.Greater(t, mock.ingressCount.Load(), int64(0))
	require.Greater(t, samplerCalls.Load(), int32(0))

	for _, key := range []string{
		"t_api_ack",
		"t_first_ping",
		"soak_exec",
		"soak_egress_probe",
		"soak_ingress_probe",
		"soak_churn",
	} {
		dist, ok := res.Latencies[key]
		require.True(t, ok, "missing latency dist %q", key)
		require.Greater(t, dist.Count, 0)
	}

	// Verify density_frontier rows and recovery_time_seconds.
	frontier, ok := res.Extra["density_frontier"].([]report.DensityTierRow)
	require.True(t, ok)
	require.Len(t, frontier, 2)
	require.Equal(t, 0.5, frontier[0].OversellRatio)
	require.Equal(t, 2, frontier[0].ActiveSandboxes)
	require.Equal(t, 128.5, frontier[0].ResidentMiBPerSandbox)
	require.Greater(t, frontier[0].RecoveryTimeSeconds, 0.0)
	require.Equal(t, 1.0, frontier[1].OversellRatio)
	require.Equal(t, 4, frontier[1].ActiveSandboxes)
	require.Greater(t, frontier[1].RecoveryTimeSeconds, 0.0)

	require.Greater(t, res.Extra["recovery_time_seconds"].(float64), 0.0)
	require.NotNil(t, res.Extra["load_profiles"])

	// All sandboxes drained.
	mock.mu.Lock()
	require.Empty(t, mock.sandboxes)
	mock.mu.Unlock()
}

func TestHighDensitySoakSuite_AdmissionDenialsAndOOMGate(t *testing.T) {
	mock, ts := newMockSoakServer(2) // deny 429 above 2 active sandboxes
	defer ts.Close()

	sampler := SoakLoadSamplerFunc(func(_ context.Context) (*SoakLoadSample, error) {
		return &SoakLoadSample{
			MemAvailableMiB: 512,
			OOMKills:        1,
		}, nil
	})

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack: profile.StackSpec{
			Template: profile.TemplateSpec{MemoryMiB: 1024},
		},
		Suites: profile.SuitesSpec{
			HighDensitySoak: &profile.HighDensitySoakSuiteSpec{
				OversellTiers:          []float64{1.0}, // fallback nodeMem=4096 -> target=4, 2 denied
				DurationSecondsPerTier: 1,
				MeasureRecovery:        true,
				SamplerIntervalSeconds: 1,
			},
		},
	}

	s := NewHighDensitySoakSuite(
		WithSoakLoadSampler(sampler),
		WithSoakPollInterval(2*time.Millisecond),
		WithSoakTickInterval(200*time.Millisecond),
	)
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-soak-oom",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)

	frontier := res.Extra["density_frontier"].([]report.DensityTierRow)
	require.Len(t, frontier, 1)
	require.Equal(t, 2, frontier[0].ActiveSandboxes)
	require.GreaterOrEqual(t, frontier[0].AdmissionDenials, 2)
	require.Equal(t, 1, frontier[0].OOMKills)
	require.Empty(t, mock.sandboxes)
}

func TestBuildEgressProbeCommand(t *testing.T) {
	cmd := buildEgressProbeCommand("http://opensandbox-server.opensandbox-system.svc:80/health")
	require.Contains(t, cmd, "command -v curl")
	require.Contains(t, cmd, "command -v wget")
	require.Contains(t, cmd, "command -v python3")
	require.Contains(t, cmd, "command -v bash")
	require.Contains(t, cmd, "/dev/tcp/")
	require.Contains(t, cmd, "'http://opensandbox-server.opensandbox-system.svc:80/health'")

	// Verify fallback order: curl -> wget -> python3 -> bash /dev/tcp.
	idxCurl := strings.Index(cmd, "command -v curl")
	idxWget := strings.Index(cmd, "command -v wget")
	idxPy := strings.Index(cmd, "command -v python3")
	idxBash := strings.Index(cmd, "command -v bash")
	require.Less(t, idxCurl, idxWget)
	require.Less(t, idxWget, idxPy)
	require.Less(t, idxPy, idxBash)
}

func TestHighDensitySoakSuite_WaitDrainChecksHookSampler(t *testing.T) {
	mock, ts := newMockSoakServer(0)
	defer ts.Close()

	var drainPolls atomic.Int32
	hookSampler := SoakLoadSamplerFunc(func(_ context.Context) (*SoakLoadSample, error) {
		n := drainPolls.Add(1)
		deleting := 0
		if n <= 3 {
			deleting = 1
		}
		return &SoakLoadSample{
			MemAvailableMiB: 2048,
			DeletingSlots:   deleting,
		}, nil
	})

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack: profile.StackSpec{
			Template: profile.TemplateSpec{MemoryMiB: 1024},
		},
		Suites: profile.SuitesSpec{
			HighDensitySoak: &profile.HighDensitySoakSuiteSpec{
				OversellTiers:          []float64{0.25},
				DurationSecondsPerTier: 1,
				MeasureRecovery:        true,
				SamplerIntervalSeconds: 5,
			},
		},
	}

	// Note: s.sampler is nil; sampler is injected solely via BenchHooks in context.
	s := NewHighDensitySoakSuite(
		WithSoakNodeMemoryMiB(4096),
		WithSoakEgressProbeURL("none"),
		WithSoakPollInterval(5*time.Millisecond),
		WithSoakTickInterval(200*time.Millisecond),
	)
	ctx := WithBenchHooks(context.Background(), BenchHooks{SoakLoadSampler: hookSampler})
	res, _, err := s.Run(ctx, RunContext{
		RunID:   "test-soak-hook-drain",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	// Hook sampler must have been polled at least 4 times (1 during tier + 3+ during drain until DeletingSlots == 0).
	require.GreaterOrEqual(t, drainPolls.Load(), int32(4))
	require.Empty(t, mock.sandboxes)
}
