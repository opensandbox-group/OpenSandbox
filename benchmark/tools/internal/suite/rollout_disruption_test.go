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

type mockRolloutServer struct {
	srv           *httptest.Server
	mu            sync.Mutex
	seq           int
	live          map[string]string // sandboxID -> stored marker
	deadSurvivors map[string]bool
	deleted       []string
	failCreates   atomic.Bool
}

func newMockRolloutServer() *mockRolloutServer {
	m := &mockRolloutServer{
		live:          make(map[string]string),
		deadSurvivors: make(map[string]bool),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if m.failCreates.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"FASTLET_UNAVAILABLE","message":"pool rolling"}`))
			return
		}
		m.mu.Lock()
		m.seq++
		id := fmt.Sprintf("sbx-rollout-%d", m.seq)
		m.live[id] = ""
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     id,
			"status": map[string]any{"state": "Running"},
		})
	})
	mux.HandleFunc("/v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
		parts := strings.Split(rest, "/")
		id := parts[0]

		if r.Method == http.MethodDelete {
			m.mu.Lock()
			delete(m.live, id)
			m.deleted = append(m.deleted, id)
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(parts) >= 3 && parts[1] == "endpoints" {
			w.Header().Set("Content-Type", "application/json")
			host := strings.TrimPrefix(m.srv.URL, "http://")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"endpoint": fmt.Sprintf("%s/execd/%s", host, id),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     id,
			"status": map[string]any{"state": "Running"},
		})
	})
	mux.HandleFunc("/execd/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/execd/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) == 2 {
			sub = "/" + parts[1]
		}

		m.mu.Lock()
		isDead := m.deadSurvivors[id]
		marker := m.live[id]
		m.mu.Unlock()

		if isDead {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		switch sub {
		case "/ping":
			w.WriteHeader(http.StatusOK)
		case "/command":
			var req opensandbox.RunCommandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			out := ""
			if strings.Contains(req.Command, "printf") {
				// Extract quoted marker between printf '%s' "<marker>" > ...
				if start := strings.Index(req.Command, `"`); start >= 0 {
					if end := strings.Index(req.Command[start+1:], `"`); end >= 0 {
						marker = req.Command[start+1 : start+1+end]
						m.mu.Lock()
						m.live[id] = marker
						m.mu.Unlock()
					}
				}
				out = marker
			} else if strings.Contains(req.Command, "cat ") {
				out = marker
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			ev1, _ := json.Marshal(map[string]any{"type": "stdout", "text": out})
			ev2, _ := json.Marshal(map[string]any{"type": "execution_complete", "exit_code": 0})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev1)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev2)
			if flusher != nil {
				flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	})

	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockRolloutServer) killOneLiveSurvivor() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, marker := range m.live {
		if marker != "" && !m.deadSurvivors[id] {
			m.deadSurvivors[id] = true
			return id
		}
	}
	return ""
}

func (m *mockRolloutServer) sdkConfig() opensandbox.ConnectionConfig {
	return opensandbox.ConnectionConfig{
		Domain:   strings.TrimPrefix(m.srv.URL, "http://"),
		Protocol: "http",
		APIKey:   "test-key",
	}
}

func (m *mockRolloutServer) close() {
	m.srv.Close()
}

type fakeRolloutTrigger struct {
	fn func(ctx context.Context, scenario string, opts RolloutTriggerOptions) (*RolloutTriggerResult, error)
}

func (f fakeRolloutTrigger) TriggerRollout(ctx context.Context, scenario string, opts RolloutTriggerOptions) (*RolloutTriggerResult, error) {
	return f.fn(ctx, scenario, opts)
}

func TestRolloutDisruptionSuite_MeasuresOutageWindowAndSurvivorDrops(t *testing.T) {
	srv := newMockRolloutServer()
	defer srv.close()

	var scenariosCalled []string
	trigger := fakeRolloutTrigger{
		fn: func(_ context.Context, scenario string, _ RolloutTriggerOptions) (*RolloutTriggerResult, error) {
			scenariosCalled = append(scenariosCalled, scenario)
			start := time.Now()
			if scenario == "pool_rollout" {
				// Simulate a ~260ms create outage window and 1 lost survivor during pool rollout.
				srv.failCreates.Store(true)
				time.Sleep(260 * time.Millisecond)
				srv.killOneLiveSurvivor()
				srv.failCreates.Store(false)
				time.Sleep(120 * time.Millisecond)
			} else {
				time.Sleep(180 * time.Millisecond)
			}
			return &RolloutTriggerResult{
				Scenario:         scenario,
				Target:           "opensandbox-dataplane/opensandbox-bench-pool",
				TargetNode:       "node-a",
				DurationSeconds:  time.Since(start).Seconds(),
				AffectedPods:     []string{"fastlet-0"},
				RolloutCompleted: true,
				Detail:           "simulated rollout complete",
			}, nil
		},
	}

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			RolloutDisruption: &profile.RolloutDisruptionSuiteSpec{
				Scenarios:               []string{"pool_rollout", "multi_fastlet_colocation", "daemonset_rollout"},
				TargetRPS:               25.0,
				BaselineSeconds:         1,
				PostRolloutSeconds:      1,
				RolloutTimeoutSeconds:   10,
				ColocationHoldSeconds:   1,
				SurvivorCount:           2,
				SurvivorProbeIntervalMs: 50,
				ReadinessPollMs:         1,
			},
		},
	}

	s := NewRolloutDisruptionSuite(WithSuiteHooks(BenchHooks{RolloutTrigger: trigger}))
	require.Equal(t, "rollout-disruption", s.Name())
	require.Equal(t, []string{profile.RuntimeFastSandboxOverlayBD}, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:     "rollout-test-1",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, samples)
	require.Equal(t, []string{"pool_rollout", "multi_fastlet_colocation", "daemonset_rollout"}, scenariosCalled)

	// Because pool_rollout experienced create failures and a survivor drop, suite status must be "failed".
	require.Equal(t, "failed", res.Status)
	require.Greater(t, res.FailedOps, 0)
	require.Equal(t, 1, res.Extra["total_survivor_drops"])
	require.GreaterOrEqual(t, res.Extra["worst_max_outage_window_ms"].(float64), 150.0)
	require.InDelta(t, 100.0, res.Extra["post_rollout_create_success_pct"].(float64), 0.1)

	phases, ok := res.Extra["phases"].([]RolloutPhaseResult)
	require.True(t, ok)
	require.Len(t, phases, 5) // baseline + 3 scenarios + post_rollout

	require.Equal(t, "baseline", phases[0].Phase)
	require.Equal(t, 0, phases[0].FailedCreates)
	require.Equal(t, 2, phases[0].SurvivorAliveAfter)

	require.Equal(t, "pool_rollout", phases[1].Phase)
	require.Greater(t, phases[1].FailedCreates, 0)
	require.GreaterOrEqual(t, phases[1].MaxOutageWindowMs, 150.0)
	require.Equal(t, 2, phases[1].SurvivorTotal)
	require.Equal(t, 1, phases[1].SurvivorAliveAfter)

	// Survivors must be replenished back to 2 before the next scenario runs.
	require.Equal(t, "multi_fastlet_colocation", phases[2].Phase)
	require.Equal(t, 2, phases[2].SurvivorTotal)
	require.Equal(t, 2, phases[2].SurvivorAliveAfter)

	require.Equal(t, "daemonset_rollout", phases[3].Phase)
	require.Equal(t, 2, phases[3].SurvivorAliveAfter)

	require.Equal(t, "post_rollout", phases[4].Phase)
	require.Equal(t, 0, phases[4].FailedCreates)

	// All live sandboxes must be cleaned up on exit.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.live)
}

func TestRolloutDisruptionSuite_ZeroDisruptionPasses(t *testing.T) {
	srv := newMockRolloutServer()
	defer srv.close()

	trigger := fakeRolloutTrigger{
		fn: func(_ context.Context, scenario string, _ RolloutTriggerOptions) (*RolloutTriggerResult, error) {
			time.Sleep(120 * time.Millisecond)
			return &RolloutTriggerResult{
				Scenario:         scenario,
				Target:           "opensandbox-system/firecracker-runtime",
				DurationSeconds:  0.12,
				RolloutCompleted: true,
			}, nil
		},
	}

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			RolloutDisruption: &profile.RolloutDisruptionSuiteSpec{
				Scenarios:               []string{"daemonset_rollout"},
				TargetRPS:               20.0,
				BaselineSeconds:         1,
				PostRolloutSeconds:      1,
				RolloutTimeoutSeconds:   5,
				SurvivorCount:           2,
				SurvivorProbeIntervalMs: 50,
				ReadinessPollMs:         1,
			},
		},
	}

	s := NewRolloutDisruptionSuite(WithSuiteHooks(BenchHooks{RolloutTrigger: trigger}))
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "rollout-zero-disruption",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, 0, res.FailedOps)
	require.Equal(t, 0, res.Extra["total_survivor_drops"])
	require.InDelta(t, 0.0, res.Extra["worst_max_outage_window_ms"].(float64), 1e-6)
}

func TestRolloutDisruptionSuite_MissingHookFailsCleanly(t *testing.T) {
	s := NewRolloutDisruptionSuite()
	res, samples, err := s.Run(context.Background(), RunContext{RunID: "no-hook"})
	require.NoError(t, err)
	require.Nil(t, samples)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.Errors["rollout_hook_unavailable"])
}

func TestComputeMaxOutageWindowMs(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	attempts := []rolloutCreateAttempt{
		{StartTime: t0, EndTime: t0.Add(50 * time.Millisecond), OK: true},
		{StartTime: t0.Add(100 * time.Millisecond), EndTime: t0.Add(200 * time.Millisecond), OK: false},
		{StartTime: t0.Add(300 * time.Millisecond), EndTime: t0.Add(450 * time.Millisecond), OK: false},
		{StartTime: t0.Add(600 * time.Millisecond), EndTime: t0.Add(650 * time.Millisecond), OK: true},
		{StartTime: t0.Add(700 * time.Millisecond), EndTime: t0.Add(800 * time.Millisecond), OK: false},
		{StartTime: t0.Add(900 * time.Millisecond), EndTime: t0.Add(950 * time.Millisecond), OK: true},
	}
	// First streak: 100ms -> 600ms = 500ms; second streak: 700ms -> 900ms = 200ms.
	require.InDelta(t, 500.0, computeMaxOutageWindowMs(attempts, t0.Add(time.Second)), 0.1)
}
