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
)

type mockWakeSandbox struct {
	id            string
	state         opensandbox.SandboxState
	idlePolicy    *opensandbox.IdlePolicy
	getPolls      int
	wakeAttempts  int
	shedOnce      bool
	parkTimeoutN  int
}

type mockWakeServer struct {
	mu           sync.Mutex
	serverURL    string
	seq          atomic.Int64
	resumeCount  atomic.Int64
	sandboxes    map[string]*mockWakeSandbox
	parkTimeouts int // number of initial 503+Retry-After responses per waking sandbox
	shedOne      bool
}

func newMockWakeServer(parkTimeouts int, shedOne bool) (*mockWakeServer, *httptest.Server) {
	m := &mockWakeServer{
		sandboxes:    make(map[string]*mockWakeSandbox),
		parkTimeouts: parkTimeouts,
		shedOne:      shedOne,
	}
	ts := httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.serverURL = ts.URL
	return m, ts
}

func (m *mockWakeServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/sandboxes":
		var req opensandbox.CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := fmt.Sprintf("wake-sbx-%d", m.seq.Add(1))
		var ip *opensandbox.IdlePolicy
		if req.Lifecycle != nil {
			ip = req.Lifecycle.IdlePolicy
		}
		m.mu.Lock()
		m.sandboxes[id] = &mockWakeSandbox{
			id:           id,
			state:        opensandbox.StateRunning,
			idlePolicy:   ip,
			parkTimeoutN: m.parkTimeouts,
			shedOnce:     m.shedOne && len(m.sandboxes) == 0,
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusAccepted, opensandbox.SandboxInfo{
			ID:        id,
			Status:    opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			CreatedAt: time.Now().UTC(),
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/")
		sbxID := parts[0]
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: fmt.Sprintf("%s/ingress/%s", m.serverURL, sbxID),
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		sbx, ok := m.sandboxes[sbxID]
		if !ok {
			m.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		sbx.getPolls++
		// Simulate auto-pause after first poll when idlePolicy is configured.
		if sbx.state == opensandbox.StateRunning && sbx.idlePolicy != nil && sbx.getPolls >= 1 {
			sbx.state = opensandbox.StatePaused
		}
		state := sbx.state
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, opensandbox.SandboxInfo{
			ID:     sbxID,
			Status: opensandbox.SandboxStatus{State: state},
		})

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/sandboxes/") && strings.HasSuffix(path, "/pause"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/sandboxes/"), "/pause")
		m.mu.Lock()
		if sbx, ok := m.sandboxes[sbxID]; ok {
			sbx.state = opensandbox.StatePaused
			sbx.wakeAttempts = 0
		}
		m.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		delete(m.sandboxes, sbxID)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/ingress/") && strings.HasSuffix(path, "/ping"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/ingress/"), "/ping")
		m.mu.Lock()
		sbx, ok := m.sandboxes[sbxID]
		if !ok {
			m.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if sbx.state == opensandbox.StatePaused {
			if sbx.shedOnce {
				sbx.shedOnce = false
				m.mu.Unlock()
				// Bare 503 without Retry-After simulates pre-resume shed.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			sbx.wakeAttempts++
			if sbx.wakeAttempts <= sbx.parkTimeoutN {
				m.mu.Unlock()
				w.Header().Set("OpenSandbox-Wake", "expired")
				w.Header().Set("Retry-After", "0.005")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// Singleflight wake: transition Paused -> Running and count 1 ResumeSandbox RPC.
			sbx.state = opensandbox.StateRunning
			m.resumeCount.Add(1)
		}
		m.mu.Unlock()

		w.Header().Set("OpenSandbox-Wake-Flight", "flight-"+sbxID)
		w.WriteHeader(http.StatusOK)

	default:
		http.NotFound(w, r)
	}
}

func TestWakeStormSuite_FanoutSingleflightAndRetryConvergence(t *testing.T) {
	mock, ts := newMockWakeServer(1, false)
	defer ts.Close()

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Environment: profile.EnvironmentSpec{
			Redis: profile.RedisSpec{Enabled: true},
		},
		Suites: profile.SuitesSpec{
			WakeStorm: &profile.WakeStormSuiteSpec{
				FleetSize:            4,
				IdleThresholdSeconds: 30,
				FanoutConcurrency:    3,
				SingleflightBurst:    6,
			},
		},
	}

	s := NewWakeStormSuite(
		WithWakePollInterval(2*time.Millisecond),
		WithWakeRetryCap(50*time.Millisecond),
		WithWakeResumeCounter(func(_ context.Context) (int64, error) {
			return mock.resumeCount.Load(), nil
		}),
	)
	require.Equal(t, "wake-storm", s.Name())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:   "test-wake-1",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Zero(t, res.FailedOps)
	require.Equal(t, 9, res.TotalOps) // 3 fanout + 6 singleflight
	require.Equal(t, 9, len(samples))

	for _, key := range []string{
		"wake_to_response_ms",
		"t_first_ping",
		"fanout_wake_to_response_ms",
		"singleflight_wake_to_response_ms",
		"sdk_retry_convergence_ms",
	} {
		dist, ok := res.Latencies[key]
		require.True(t, ok, "missing latency dist %q", key)
		require.Greater(t, dist.Count, 0)
	}

	// Singleflight coalesced 6 concurrent requests into 1 ResumeSandbox RPC.
	require.Equal(t, "fastpath_resume_counter", res.Extra["singleflight_source"])
	require.InDelta(t, 6.0, res.Extra["singleflight_coalesce_ratio"].(float64), 0.01)
	require.Greater(t, res.Extra["wake_park_timeout_rate"].(float64), 0.0)
	require.Equal(t, 0.0, res.Extra["wake_shed_rate"].(float64))
	require.Greater(t, res.Extra["retry_count_total"].(int), 0)

	// Cleanup verified.
	mock.mu.Lock()
	require.Empty(t, mock.sandboxes)
	mock.mu.Unlock()
}

func TestWakeStormSuite_TracksShedRate(t *testing.T) {
	mock, ts := newMockWakeServer(0, true)
	defer ts.Close()

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Suites: profile.SuitesSpec{
			WakeStorm: &profile.WakeStormSuiteSpec{
				FleetSize:            2,
				IdleThresholdSeconds: 30,
				FanoutConcurrency:    2,
				SingleflightBurst:    2,
			},
		},
	}

	s := NewWakeStormSuite(WithWakePollInterval(2 * time.Millisecond))
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-wake-shed",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Greater(t, res.Extra["wake_shed_count"].(int), 0)
	require.Greater(t, res.Extra["wake_shed_rate"].(float64), 0.0)
	require.Greater(t, res.Errors["wake_shed_503"], 0)
	require.Empty(t, mock.sandboxes)
}

func TestWakeStormSuite_ZeroDeltaCounterFallsBackToFlightHeader(t *testing.T) {
	mock, ts := newMockWakeServer(0, false)
	defer ts.Close()

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Suites: profile.SuitesSpec{
			WakeStorm: &profile.WakeStormSuiteSpec{
				FleetSize:            2,
				IdleThresholdSeconds: 30,
				FanoutConcurrency:    1,
				SingleflightBurst:    4,
			},
		},
	}

	// Simulate a missing/unchanging counter returning constant 0: must fall back to flight_header, not fake delta=1.
	s := NewWakeStormSuite(
		WithWakePollInterval(2*time.Millisecond),
		WithWakeResumeCounter(func(_ context.Context) (int64, error) {
			return 0, nil
		}),
	)
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-wake-fallback",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, "flight_header", res.Extra["singleflight_source"])
	require.InDelta(t, 4.0, res.Extra["singleflight_coalesce_ratio"].(float64), 0.01)
	require.Empty(t, mock.sandboxes)
}
