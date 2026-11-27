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

type mockMicrobenchServer struct {
	server *httptest.Server

	mu         sync.Mutex
	seq        atomic.Int64
	liveIDs    map[string]bool
	deletedIDs map[string]bool
	onExec     func(sandboxID, command string, background bool) (stdout string, exitCode int)
}

func newMockMicrobenchServer(onExec func(sandboxID, command string, background bool) (string, int)) *mockMicrobenchServer {
	m := &mockMicrobenchServer{
		liveIDs:    make(map[string]bool),
		deletedIDs: make(map[string]bool),
		onExec:     onExec,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/templates":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opensandbox.ListTemplatesResponse{
				Items: []opensandbox.TemplateInfo{{
					TemplateID: "tpl-microbench",
					Image:      "python:3.12-bookworm",
					Status:     opensandbox.TemplateStatus{Phase: opensandbox.TemplatePhaseSucceeded},
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			id := fmt.Sprintf("mb-sbx-%d", m.seq.Add(1))
			m.mu.Lock()
			m.liveIDs[id] = true
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(opensandbox.SandboxInfo{
				ID:     id,
				Status: opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") && strings.Contains(r.URL.Path, "/endpoints/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/")
			sbxID := parts[0]
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opensandbox.Endpoint{
				Endpoint: m.server.URL,
				Headers:  map[string]string{"X-Mock-Sandbox-ID": sbxID},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("pong"))
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			var req opensandbox.RunCommandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			sbxID := r.Header.Get("X-Mock-Sandbox-ID")
			stdout, code := m.onExec(sbxID, req.Command, req.Background)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if stdout != "" {
				ev, _ := json.Marshal(map[string]any{"type": "stdout", "text": stdout})
				fmt.Fprintf(w, "data: %s\n\n", ev)
			}
			doneEv, _ := json.Marshal(map[string]any{"type": "execution_complete", "exit_code": code})
			fmt.Fprintf(w, "data: %s\n\n", doneEv)
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

func (m *mockMicrobenchServer) close() { m.server.Close() }

func (m *mockMicrobenchServer) sdkConfig() opensandbox.ConnectionConfig {
	base := m.server.Client()
	httpClient := &http.Client{Timeout: 5 * time.Second}
	if tr, ok := base.Transport.(*http.Transport); ok {
		cloned := tr.Clone()
		cloned.Proxy = nil
		cloned.MaxIdleConns = 64
		cloned.MaxIdleConnsPerHost = 64
		httpClient.Transport = cloned
	}
	return opensandbox.ConnectionConfig{
		Domain:         strings.TrimPrefix(m.server.URL, "http://"),
		Protocol:       "http",
		HTTPClient:     httpClient,
		RequestTimeout: 5 * time.Second,
		DisableMetrics: true,
	}
}

func TestDataplaneMicrobenchSuite_SysbenchFioHTTPProbe(t *testing.T) {
	srv := newMockMicrobenchServer(func(sandboxID, command string, _ bool) (string, int) {
		switch {
		case strings.Contains(command, "sysbench"):
			eps := 1800.0
			if sandboxID != "mb-sbx-1" {
				eps = 1620.0
			}
			return fmt.Sprintf(`{"tool":"sysbench","events_per_sec":%.1f,"total_events":9000,"elapsed_ms":12.5}`, eps), 0
		case strings.Contains(command, "fio"):
			return `{"tool":"fio","file_size_mib":16,"rand_read_4k_iops":24500,"rand_write_4k_iops":14200,"seq_read_1m_cold_mibs":420.5,"seq_read_1m_hot_mibs":2150.0,"seq_write_1m_mibs":310.2,"elapsed_ms":45.0}`, 0
		default:
			return `{"tool":"http-probe","target":"http://server/health","concurrency":4,"requests":20,"succeeded":20,"failed":0,"rps":850.5,"throughput_mbps":12.4,"rtt_p50_ms":2.1,"rtt_p95_ms":4.5,"rtt_p99_ms":6.2,"rtt_mean_ms":2.4,"rtt_samples_ms":[1.5,2.1,3.2,4.5,6.2],"elapsed_ms":23.5}`, 0
		}
	})
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			DataplaneMicrobench: &profile.DataplaneMicrobenchSuiteSpec{
				ConcurrentSandboxes: 3,
				CPUSeconds:          2,
				StorageFileSizeMiB:  16,
				NetworkRequests:     20,
				NetworkConcurrency:  4,
				ReadinessPollMs:     1,
			},
		},
	}

	s := NewDataplaneMicrobenchSuite()
	require.Equal(t, "dataplane-microbench", s.Name())
	require.Nil(t, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:     "microbench-test-1",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Equal(t, 0, res.FailedOps)
	require.NotEmpty(t, samples)

	// Verify CPU single vs concurrent metrics.
	cpuMap, ok := res.Extra["cpu"].(map[string]any)
	require.True(t, ok)
	single := cpuMap["single"].(*CPUMicrobenchResult)
	require.Equal(t, "sysbench", single.Tool)
	require.InDelta(t, 1800.0, single.EventsPerSec, 0.1)

	conc := cpuMap["concurrent"].(*CPUConcurrentSummary)
	require.Equal(t, 3, conc.SandboxCount)
	require.Equal(t, 3, conc.SucceededCount)
	require.InDelta(t, 1620.0, conc.MeanEventsPerSec, 0.1)
	require.InDelta(t, 0.9, conc.ScalingEfficiencyRatio, 0.01)

	// Verify Storage metrics.
	storage := res.Extra["storage"].(*StorageMicrobenchResult)
	require.Equal(t, "fio", storage.Tool)
	require.InDelta(t, 24500, storage.RandRead4KIOPS, 0.1)
	require.InDelta(t, 420.5, storage.SeqRead1MColdMiBs, 0.1)
	require.InDelta(t, 2150.0, storage.SeqRead1MHotMiBs, 0.1)

	// Verify Network metrics & latencies.
	netRes := res.Extra["network"].(*NetworkMicrobenchResult)
	require.Equal(t, "http-probe", netRes.Tool)
	require.InDelta(t, 6.2, netRes.RTTP99Ms, 0.01)
	require.Equal(t, 5, res.Latencies["network_rtt_ms"].Count)
	require.Equal(t, 4, res.Latencies["t_api_ack"].Count)

	// All 4 sandboxes (1 baseline + 3 concurrent) must be deleted.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs)
	require.Len(t, srv.deletedIDs, 4)
}

func TestDataplaneMicrobenchSuite_FallbackToolsAndIperf3(t *testing.T) {
	srv := newMockMicrobenchServer(func(_, command string, _ bool) (string, int) {
		switch {
		case strings.Contains(command, "sysbench"):
			return `{"tool":"python3-fallback","ops_per_sec":420.0,"events_per_sec":420.0,"total_events":840,"elapsed_ms":2000.0}`, 0
		case strings.Contains(command, "fio"):
			return `{"tool":"python3-dd-fallback","file_size_mib":8,"rand_read_4k_iops":8200,"rand_write_4k_iops":3100,"seq_read_1m_cold_mibs":280.0,"seq_read_1m_hot_mibs":1800.0,"seq_write_1m_mibs":210.0,"elapsed_ms":120.0}`, 0
		case strings.Contains(command, "iperf3"):
			return `{"tool":"iperf3","target":"10.0.0.5","concurrency":2,"throughput_mbps":9420.5,"elapsed_ms":2010.0}`, 0
		default:
			return `{}`, 1
		}
	})
	defer srv.close()

	prof := &profile.BenchmarkProfile{
		Suites: profile.SuitesSpec{
			DataplaneMicrobench: &profile.DataplaneMicrobenchSuiteSpec{
				ConcurrentSandboxes: 1,
				CPUSeconds:          1,
				StorageFileSizeMiB:  8,
				Iperf3Server:        "10.0.0.5",
				NetworkConcurrency:  2,
				ReadinessPollMs:     1,
			},
		},
	}

	s := NewDataplaneMicrobenchSuite()
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:     "microbench-fallback",
		Profile:   prof,
		SDKConfig: srv.sdkConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)

	cpuMap := res.Extra["cpu"].(map[string]any)
	require.Equal(t, "python3-fallback", cpuMap["single"].(*CPUMicrobenchResult).Tool)
	require.NotContains(t, cpuMap, "concurrent")

	storage := res.Extra["storage"].(*StorageMicrobenchResult)
	require.Equal(t, "python3-dd-fallback", storage.Tool)

	netRes := res.Extra["network"].(*NetworkMicrobenchResult)
	require.Equal(t, "iperf3", netRes.Tool)
	require.InDelta(t, 9420.5, netRes.ThroughputMbps, 0.1)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.liveIDs)
	require.Len(t, srv.deletedIDs, 1)
}
