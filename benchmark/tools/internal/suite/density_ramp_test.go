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

type mockDensityServer struct {
	server *httptest.Server

	mu            sync.Mutex
	createdIDs    []string
	liveIDs       map[string]bool
	deletedIDs    map[string]bool
	maxConcurrent int
	pingCalls     int

	createSeq     atomic.Int64
	failAfter     int64 // if > 0, fail CreateSandbox calls after this many creates
	onCreate      func(seq int64)
}

func newMockDensityServer(failAfter int64, onCreate func(seq int64)) *mockDensityServer {
	m := &mockDensityServer{
		liveIDs:    make(map[string]bool),
		deletedIDs: make(map[string]bool),
		failAfter:  failAfter,
		onCreate:   onCreate,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			seq := m.createSeq.Add(1)
			if m.onCreate != nil {
				m.onCreate(seq)
			}
			if m.failAfter > 0 && seq > m.failAfter {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "CAPACITY_EXCEEDED", "message": "pool full"})
				return
			}
			id := fmt.Sprintf("sbx-%d", seq)
			m.mu.Lock()
			m.createdIDs = append(m.createdIDs, id)
			m.liveIDs[id] = true
			if len(m.liveIDs) > m.maxConcurrent {
				m.maxConcurrent = len(m.liveIDs)
			}
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(opensandbox.SandboxInfo{
				ID:     id,
				Status: opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") && strings.Contains(r.URL.Path, "/endpoints/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opensandbox.Endpoint{
				Endpoint: m.server.URL,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			m.mu.Lock()
			m.pingCalls++
			m.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("pong"))

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
			m.mu.Lock()
			delete(m.liveIDs, id)
			m.deletedIDs[id] = true
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return m
}

func (m *mockDensityServer) close() { m.server.Close() }

func TestDensityRampStepProgressionAndCleanup(t *testing.T) {
	srv := newMockDensityServer(0, nil)
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			DensityRamp: &profile.DensityRampSuiteSpec{
				Steps:                  []int{2, 5, 8},
				BatchCreateConcurrency: 4,
				MaxBatchFailureRate:    0.05,
				PingSampleSize:         3,
				ReadinessPollMs:        1,
			},
		},
	}

	s := NewDensityRampSuite(WithDensityRampPollInterval(time.Millisecond))
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "density-test-1",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(srv.server.URL, "http://"),
			Protocol:   "http",
			HTTPClient: srv.server.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, 8, res.Extra["peak_active_sandboxes"])
	require.Equal(t, false, res.Extra["early_stopped"])

	steps, ok := res.Extra["steps"].([]DensityRampStepResult)
	require.True(t, ok)
	require.Len(t, steps, 3)
	require.Equal(t, 2, steps[0].LiveSandboxes)
	require.Equal(t, 2, steps[0].PingSampled)
	require.Equal(t, 5, steps[1].LiveSandboxes)
	require.Equal(t, 3, steps[1].PingSampled)
	require.Equal(t, 8, steps[2].LiveSandboxes)
	require.Equal(t, 3, steps[2].PingSampled)

	// Verify sandboxes stayed alive across steps (maxConcurrent == 8) and ALL 8 were cleaned up on exit.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, 8, srv.maxConcurrent)
	require.Empty(t, srv.liveIDs, "all live sandboxes must be deleted in defer cleanup")
	require.Len(t, srv.deletedIDs, 8)
}

func TestDensityRampEarlySafetyStopAndGuaranteedCleanup(t *testing.T) {
	// Allow step 1 (4 sandboxes) + 1 sandbox in step 2, then fail remaining creates so
	// step 2 failure rate is 5/6 (83%) > 5% threshold.
	srv := newMockDensityServer(5, nil)
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			DensityRamp: &profile.DensityRampSuiteSpec{
				Steps:                  []int{4, 10, 20},
				BatchCreateConcurrency: 4,
				MaxBatchFailureRate:    0.05,
				PingSampleSize:         4,
				ReadinessPollMs:        1,
			},
		},
	}

	s := NewDensityRampSuite(WithDensityRampPollInterval(time.Millisecond))
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "density-stop-test",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(srv.server.URL, "http://"),
			Protocol:   "http",
			HTTPClient: srv.server.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, true, res.Extra["early_stopped"])
	require.NotEmpty(t, res.Extra["stop_reason"])

	steps, ok := res.Extra["steps"].([]DensityRampStepResult)
	require.True(t, ok)
	// Must stop at step 2 and never attempt step 3 (20).
	require.Len(t, steps, 2)
	require.False(t, steps[0].StoppedEarly)
	require.True(t, steps[1].StoppedEarly)
	require.Greater(t, steps[1].BatchFailureRate, 0.05)

	// All 5 created sandboxes (4 from step 1 + 1 from step 2) must be cleaned up.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs, "all created sandboxes must be cleaned up after early safety stop")
	require.Len(t, srv.deletedIDs, 5)
}

func TestDensityRampContextCancelGuaranteedCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel context after 3 creates so step 2 aborts mid-way.
	srv := newMockDensityServer(0, func(seq int64) {
		if seq == 3 {
			cancel()
		}
	})
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			DensityRamp: &profile.DensityRampSuiteSpec{
				Steps:                  []int{2, 6},
				BatchCreateConcurrency: 1,
				MaxBatchFailureRate:    0.05,
				PingSampleSize:         2,
				ReadinessPollMs:        1,
			},
		},
	}

	s := NewDensityRampSuite(WithDensityRampPollInterval(time.Millisecond))
	res, _, err := s.Run(ctx, RunContext{
		RunID:   "density-cancel-test",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(srv.server.URL, "http://"),
			Protocol:   "http",
			HTTPClient: srv.server.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs, "all created sandboxes must be cleaned up even on context cancellation")
	require.Len(t, srv.deletedIDs, len(srv.createdIDs))
}
