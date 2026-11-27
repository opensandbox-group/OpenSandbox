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
	"regexp"
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

type mockSandbox struct {
	id           string
	state        opensandbox.SandboxState
	node         string
	diskMarker   string
	memMarker    string
	roundMarkers map[string]string
	pauseCount   int
}

type mockBenchBackend struct {
	server *httptest.Server
	execd  *httptest.Server

	mu            sync.Mutex
	seq           atomic.Int64
	sandboxes     map[string]*mockSandbox
	deletedCount  atomic.Int64
	isolatedNodes map[string]bool
	nodes         []string

	// Configurable fault / latency injection hooks for tests.
	pendingEveryN          int // if > 0, every Nth create returns Pending initially
	rate429EveryN          int // if > 0, every Nth create returns 429 + Retry-After
	pauseDelayFn           func(sbx *mockSandbox, round int) time.Duration
	failMemVerify          bool
	failRoundMarker        bool
	ignoreIsolation        bool // if true, cross-node resume stays on same node (negative test)
	verifyExec503Remaining atomic.Int32
	verifyExecAttempts     atomic.Int32
}

var (
	reDiskWrite  = regexp.MustCompile(`printf '%s' '([^']+)' > /tmp/bench-disk-marker`)
	reMemStart   = regexp.MustCompile(`export BENCH_MEM_MARKER=([^\s;']+)`)
	reRoundWrite = regexp.MustCompile(`printf '%s' '([^']+)' > (/tmp/bench-round-\d+\.dat)`)
	reRoundRead  = regexp.MustCompile(`cat (/tmp/bench-round-\d+\.dat)`)
)

func newMockBenchBackend(t *testing.T) *mockBenchBackend {
	t.Helper()
	b := &mockBenchBackend{
		sandboxes:     make(map[string]*mockSandbox),
		isolatedNodes: make(map[string]bool),
		nodes:         []string{"node-a", "node-b"},
	}

	b.execd = httptest.NewServer(http.HandlerFunc(b.handleExecd))
	b.server = httptest.NewServer(http.HandlerFunc(b.handleServer))
	t.Cleanup(func() {
		b.server.Close()
		b.execd.Close()
	})
	return b
}

func (b *mockBenchBackend) sdkConfig() opensandbox.ConnectionConfig {
	return opensandbox.ConnectionConfig{
		Domain:         b.server.URL,
		Protocol:       "http",
		APIKey:         "test-api-key",
		RequestTimeout: 5 * time.Second,
		DisableMetrics: true,
	}
}

func (b *mockBenchBackend) activeSandboxCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sandboxes)
}

func (b *mockBenchBackend) pickNodeLocked() string {
	for _, n := range b.nodes {
		if !b.isolatedNodes[n] {
			return n
		}
	}
	return b.nodes[0]
}

func (b *mockBenchBackend) handleServer(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == "/v1/templates":
		writeJSON(w, http.StatusOK, opensandbox.ListTemplatesResponse{
			Items: []opensandbox.TemplateInfo{
				{
					TemplateID: "tpl-overlaybd-golden",
					Image:      "python:3.12-bookworm",
					Format:     opensandbox.TemplateFormatOverlayBD,
					Status: opensandbox.TemplateStatus{
						Phase:       opensandbox.TemplatePhaseSucceeded,
						ManifestRef: "oci://opensandbox-bench-registry.opensandbox-bench.svc/templates/golden@sha256:abc",
					},
				},
			},
		})
		return

	case r.Method == http.MethodPost && path == "/v1/sandboxes":
		n := int(b.seq.Add(1))
		if b.rate429EveryN > 0 && n%b.rate429EveryN == 0 {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, opensandbox.ErrorResponse{
				Code:    "RATE_LIMITED",
				Message: "too many concurrent creates",
			})
			return
		}

		id := fmt.Sprintf("sbx-%d", n)
		initialState := opensandbox.StateRunning
		if b.pendingEveryN > 0 && n%b.pendingEveryN == 0 {
			initialState = opensandbox.StatePending
		}

		b.mu.Lock()
		node := b.pickNodeLocked()
		sbx := &mockSandbox{
			id:    id,
			state: initialState,
			node:  node,
		}
		b.sandboxes[id] = sbx
		b.mu.Unlock()

		if initialState == opensandbox.StatePending {
			go func() {
				time.Sleep(15 * time.Millisecond)
				b.mu.Lock()
				if cur, ok := b.sandboxes[id]; ok && cur.state == opensandbox.StatePending {
					cur.state = opensandbox.StateRunning
				}
				b.mu.Unlock()
			}()
		}

		writeJSON(w, http.StatusCreated, opensandbox.SandboxInfo{
			ID:     id,
			Status: opensandbox.SandboxStatus{State: initialState},
			Extensions: map[string]string{
				"fast-sandbox.io/node": node,
			},
			CreatedAt: time.Now().UTC(),
		})
		return
	}

	if !strings.HasPrefix(path, "/v1/sandboxes/") {
		http.NotFound(w, r)
		return
	}

	rest := strings.TrimPrefix(path, "/v1/sandboxes/")
	parts := strings.Split(rest, "/")
	id := parts[0]

	// GET /v1/sandboxes/{id}
	if len(parts) == 1 && r.Method == http.MethodGet {
		b.mu.Lock()
		sbx, ok := b.sandboxes[id]
		if !ok {
			b.mu.Unlock()
			writeJSON(w, http.StatusNotFound, opensandbox.ErrorResponse{Code: "NOT_FOUND", Message: "sandbox not found"})
			return
		}
		info := opensandbox.SandboxInfo{
			ID:     sbx.id,
			Status: opensandbox.SandboxStatus{State: sbx.state},
			Extensions: map[string]string{
				"fast-sandbox.io/node": sbx.node,
			},
		}
		b.mu.Unlock()
		writeJSON(w, http.StatusOK, info)
		return
	}

	// DELETE /v1/sandboxes/{id}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		b.mu.Lock()
		delete(b.sandboxes, id)
		b.mu.Unlock()
		b.deletedCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// GET /v1/sandboxes/{id}/endpoints/{port}
	if len(parts) == 3 && parts[1] == "endpoints" && r.Method == http.MethodGet {
		b.mu.Lock()
		sbx, ok := b.sandboxes[id]
		state := opensandbox.StateTerminated
		if ok {
			state = sbx.state
		}
		b.mu.Unlock()
		if !ok || state != opensandbox.StateRunning {
			writeJSON(w, http.StatusNotFound, opensandbox.ErrorResponse{
				Code:    "KUBERNETES::POD_IP_NOT_AVAILABLE",
				Message: "sandbox not running yet",
			})
			return
		}
		w.Header().Set(opensandbox.SandboxOriginHeader, string(opensandbox.SandboxOriginTemplate))
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: b.execd.URL + "/sbx/" + id,
		})
		return
	}

	// POST /v1/sandboxes/{id}/pause
	if len(parts) == 2 && parts[1] == "pause" && r.Method == http.MethodPost {
		b.mu.Lock()
		sbx, ok := b.sandboxes[id]
		if !ok {
			b.mu.Unlock()
			writeJSON(w, http.StatusNotFound, opensandbox.ErrorResponse{Code: "NOT_FOUND", Message: "sandbox not found"})
			return
		}
		sbx.state = opensandbox.StatePausing
		sbx.pauseCount++
		round := sbx.pauseCount
		delay := 5 * time.Millisecond
		if b.pauseDelayFn != nil {
			delay = b.pauseDelayFn(sbx, round)
		}
		b.mu.Unlock()

		go func() {
			time.Sleep(delay)
			b.mu.Lock()
			if cur, exists := b.sandboxes[id]; exists && cur.state == opensandbox.StatePausing {
				cur.state = opensandbox.StatePaused
			}
			b.mu.Unlock()
		}()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// POST /v1/sandboxes/{id}/resume
	if len(parts) == 2 && parts[1] == "resume" && r.Method == http.MethodPost {
		b.mu.Lock()
		sbx, ok := b.sandboxes[id]
		if !ok {
			b.mu.Unlock()
			writeJSON(w, http.StatusNotFound, opensandbox.ErrorResponse{Code: "NOT_FOUND", Message: "sandbox not found"})
			return
		}
		if !b.ignoreIsolation && b.isolatedNodes[sbx.node] {
			sbx.node = b.pickNodeLocked()
		}
		sbx.state = opensandbox.StateRunning
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	http.NotFound(w, r)
}

func (b *mockBenchBackend) handleExecd(w http.ResponseWriter, r *http.Request) {
	// Path format: /sbx/{id}/ping or /sbx/{id}/command
	trimmed := strings.TrimPrefix(r.URL.Path, "/sbx/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, sub := parts[0], parts[1]

	b.mu.Lock()
	sbx, ok := b.sandboxes[id]
	running := ok && sbx.state == opensandbox.StateRunning
	b.mu.Unlock()
	if !running {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	switch sub {
	case "ping":
		w.WriteHeader(http.StatusOK)
		return

	case "command":
		var req opensandbox.RunCommandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stdout := "ok"
		b.mu.Lock()
		if m := reDiskWrite.FindStringSubmatch(req.Command); len(m) == 2 {
			sbx.diskMarker = m[1]
			stdout = "wrote"
		} else if m := reMemStart.FindStringSubmatch(req.Command); len(m) == 2 {
			sbx.memMarker = m[1]
			stdout = "started"
		} else if m := reRoundWrite.FindStringSubmatch(req.Command); len(m) == 3 {
			if sbx.roundMarkers == nil {
				sbx.roundMarkers = make(map[string]string)
			}
			sbx.roundMarkers[m[2]] = m[1]
			stdout = "wrote-round"
		} else if m := reRoundRead.FindStringSubmatch(req.Command); len(m) == 2 {
			if b.failRoundMarker {
				stdout = "corrupted-marker"
			} else if sbx.roundMarkers != nil {
				stdout = sbx.roundMarkers[m[1]]
			} else {
				stdout = ""
			}
		} else if strings.Contains(req.Command, "DISK=$DISK MEM=$MEM_ALIVE") {
			b.verifyExecAttempts.Add(1)
			if b.verifyExec503Remaining.Add(-1) >= 0 {
				b.mu.Unlock()
				writeJSON(w, http.StatusServiceUnavailable, opensandbox.ErrorResponse{
					Code:    "UPSTREAM_UNAVAILABLE",
					Message: "route settling after resume",
				})
				return
			}
			memAlive := "yes"
			if b.failMemVerify || sbx.memMarker == "" {
				memAlive = "no"
			}
			stdout = fmt.Sprintf("DISK=%s MEM=%s", sbx.diskMarker, memAlive)
		}
		b.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"type\":\"stdout\",\"text\":%q}\n\n", stdout)
		fmt.Fprintf(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		return
	}

	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type mockCacheEvictor struct {
	mu    sync.Mutex
	nodes []string
}

func (m *mockCacheEvictor) EvictOverlayBDCaches(_ context.Context, nodeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes = append(m.nodes, nodeName)
	return nil
}

type mockNodeIsolator struct {
	backend  *mockBenchBackend
	mu       sync.Mutex
	isolated []string
	restored []string
}

func (m *mockNodeIsolator) IsolateNodeFastlets(_ context.Context, nodeName string) (func(context.Context) error, error) {
	m.backend.mu.Lock()
	m.backend.isolatedNodes[nodeName] = true
	m.backend.mu.Unlock()

	m.mu.Lock()
	m.isolated = append(m.isolated, nodeName)
	m.mu.Unlock()

	return func(context.Context) error {
		m.backend.mu.Lock()
		delete(m.backend.isolatedNodes, nodeName)
		m.backend.mu.Unlock()

		m.mu.Lock()
		m.restored = append(m.restored, nodeName)
		m.mu.Unlock()
		return nil
	}, nil
}

type mockMetricsSnapshotter struct {
	beforeCalled bool
	afterCalled  bool
}

func (m *mockMetricsSnapshotter) BeforeSuite(_ context.Context, _ string) error {
	m.beforeCalled = true
	return nil
}

func (m *mockMetricsSnapshotter) AfterSuite(_ context.Context, _ string) (*report.OverlayBDMetrics, map[string]float64, error) {
	m.afterCalled = true
	return &report.OverlayBDMetrics{
		UblkdPoolHitsDelta: 10,
		UblkdPoolHitRate:   1.0,
	}, map[string]float64{"fast_sandbox_image_affinity_hit": 10}, nil
}

func testOverlayBDProfile() *profile.BenchmarkProfile {
	p, err := profile.LoadEmbedded(profile.DefaultProfileName)
	if err != nil {
		panic(err)
	}
	return p
}

func TestBenchClient_CreatePauseResumeDelete(t *testing.T) {
	backend := newMockBenchBackend(t)
	backend.pendingEveryN = 2 // 2nd create returns Pending first

	client := NewBenchClient(backend.sdkConfig())
	ctx := context.Background()

	tplID, err := client.ResolveTemplateID(ctx, testOverlayBDProfile())
	require.NoError(t, err)
	require.Equal(t, "tpl-overlaybd-golden", tplID)

	// 1st create: synchronous Running ack
	r1, err := client.CreateSandbox(ctx, CreateOptions{
		TemplateID:            tplID,
		ReadinessPollInterval: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Equal(t, "Running", r1.AckState)
	require.GreaterOrEqual(t, r1.FirstPingMs, r1.APIAckMs)
	require.GreaterOrEqual(t, r1.FirstExecMs, r1.FirstPingMs)

	node, err := client.GetSandboxNode(ctx, r1.SandboxID)
	require.NoError(t, err)
	require.Equal(t, "node-a", node)

	// Pause & Resume
	pRes, err := client.Pause(ctx, r1.SandboxID, 5*time.Millisecond, 5*time.Second)
	require.NoError(t, err)
	require.GreaterOrEqual(t, pRes.TotalMs, pRes.APIAckMs)

	resRes, err := client.Resume(ctx, r1.SandboxID, 5*time.Millisecond, 5*time.Second)
	require.NoError(t, err)
	require.GreaterOrEqual(t, resRes.FirstPingMs, resRes.APIAckMs)

	require.NoError(t, client.Delete(ctx, r1.SandboxID))

	// 2nd create: Pending fallback ack, converges asynchronously to Running + ping
	r2, err := client.CreateSandbox(ctx, CreateOptions{
		TemplateID:            tplID,
		ReadinessPollInterval: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Equal(t, "Pending", r2.AckState)
	require.Greater(t, r2.FirstPingMs, r2.APIAckMs)
	require.NoError(t, client.Delete(ctx, r2.SandboxID))
	require.Equal(t, 0, backend.activeSandboxCount())
}

func TestBenchClient_429RetryAfterDetection(t *testing.T) {
	backend := newMockBenchBackend(t)
	backend.rate429EveryN = 1

	client := NewBenchClient(backend.sdkConfig())
	cRes, err := client.CreateSandbox(context.Background(), CreateOptions{
		TemplateID: "tpl-overlaybd-golden",
	})
	require.Error(t, err)
	require.NotNil(t, cRes)
	require.True(t, cRes.Is429)
	require.Equal(t, http.StatusTooManyRequests, cRes.StatusCode)
	require.Equal(t, "http_429", cRes.ErrorCode)
	require.Equal(t, time.Second, cRes.RetryAfter)
}
