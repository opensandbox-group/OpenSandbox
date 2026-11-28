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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

type mockRootfsSandbox struct {
	id         string
	node       string
	vcpus      int
	memMiB     int
	entrypoint []string
	marker     string
	deleted    bool
}

type mockRootfsServer struct {
	mu                   sync.Mutex
	serverURL            string
	seq                  atomic.Int64
	snapSeq              atomic.Int64
	sandboxes            map[string]*mockRootfsSandbox
	snapshots            map[string]string // snapID -> marker
	isolatedNode         string
	ignoreScaleDown      bool
	captureStallDuration time.Duration
	stallingSandboxID    string
	stallUntil           time.Time
}

func newMockRootfsServer() (*mockRootfsServer, *httptest.Server) {
	m := &mockRootfsServer{
		sandboxes: make(map[string]*mockRootfsSandbox),
		snapshots: make(map[string]string),
	}
	ts := httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.serverURL = ts.URL
	return m, ts
}

func parseMockLimits(limits opensandbox.ResourceLimits, defaultVCPUs, defaultMemMiB int) (int, int) {
	vcpus := defaultVCPUs
	memMiB := defaultMemMiB
	if raw, ok := limits["cpu"]; ok && raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v > 0 {
			vcpus = v
		}
	}
	if raw, ok := limits["memory"]; ok && raw != "" {
		trimmed := strings.TrimSuffix(strings.TrimSpace(raw), "Mi")
		if v, err := strconv.Atoi(trimmed); err == nil && v > 0 {
			memMiB = v
		}
	}
	return vcpus, memMiB
}

func (m *mockRootfsServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/sandboxes":
		var req opensandbox.CreateSandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		m.mu.Lock()
		id := fmt.Sprintf("rootfs-sbx-%d", m.seq.Add(1))
		node := "node-a"
		if m.isolatedNode == "node-a" {
			node = "node-b"
		}
		vcpus, memMiB := parseMockLimits(req.ResourceLimits, 2, 2048)
		if m.ignoreScaleDown && req.SnapshotID != "" && memMiB < 2048 {
			memMiB = 2048
		}
		marker := ""
		if req.SnapshotID != "" {
			marker = m.snapshots[req.SnapshotID]
		}
		m.sandboxes[id] = &mockRootfsSandbox{
			id:         id,
			node:       node,
			vcpus:      vcpus,
			memMiB:     memMiB,
			entrypoint: append([]string(nil), req.Entrypoint...),
			marker:     marker,
		}
		m.mu.Unlock()

		writeJSON(w, http.StatusAccepted, opensandbox.SandboxInfo{
			ID:        id,
			Status:    opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			Metadata:  map[string]string{"nodeName": node},
			CreatedAt: time.Now().UTC(),
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && !strings.Contains(strings.TrimPrefix(path, "/v1/sandboxes/"), "/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		sbx := m.sandboxes[sbxID]
		deleted := sbx == nil || sbx.deleted
		m.mu.Unlock()
		if deleted {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "sandbox deleted"})
			return
		}
		writeJSON(w, http.StatusOK, opensandbox.SandboxInfo{
			ID:       sbx.id,
			Status:   opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			Metadata: map[string]string{"nodeName": sbx.node},
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		if sbx := m.sandboxes[sbxID]; sbx != nil {
			sbx.deleted = true
		}
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/")
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: fmt.Sprintf("%s/execd/%s", m.serverURL, parts[0]),
		})

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/sandboxes/") && strings.HasSuffix(path, "/snapshots"):
		trimmed := strings.TrimPrefix(path, "/v1/sandboxes/")
		sbxID := strings.TrimSuffix(trimmed, "/snapshots")
		var req opensandbox.CreateSnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		m.mu.Lock()
		sbx := m.sandboxes[sbxID]
		snapID := fmt.Sprintf("snap-%d", m.snapSeq.Add(1))
		if sbx != nil {
			m.snapshots[snapID] = sbx.marker
		}
		stallDur := m.captureStallDuration
		if stallDur > 0 {
			m.stallingSandboxID = sbxID
			m.stallUntil = time.Now().Add(stallDur)
		}
		m.mu.Unlock()

		if stallDur > 0 {
			time.Sleep(stallDur + 30*time.Millisecond)
		}

		writeJSON(w, http.StatusAccepted, opensandbox.SnapshotInfo{
			ID:     snapID,
			Name:   req.Name,
			Status: opensandbox.SnapshotStatus{State: opensandbox.SnapshotStateReady},
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/snapshots/"):
		snapID := strings.TrimPrefix(path, "/v1/snapshots/")
		writeJSON(w, http.StatusOK, opensandbox.SnapshotInfo{
			ID:     snapID,
			Status: opensandbox.SnapshotStatus{State: opensandbox.SnapshotStateReady},
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/snapshots/"):
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/ping"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/execd/"), "/ping")
		m.mu.Lock()
		stalling := m.stallingSandboxID == sbxID && time.Now().Before(m.stallUntil)
		m.mu.Unlock()
		if stalling {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/command"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/execd/"), "/command")
		var req opensandbox.RunCommandRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		m.mu.Lock()
		sbx := m.sandboxes[sbxID]
		m.mu.Unlock()
		if sbx == nil {
			http.NotFound(w, r)
			return
		}

		cmd := req.Command
		switch {
		case strings.Contains(cmd, "> /var/tmp/bench-rootfs-marker"):
			// Extract token between printf '%s' "..." > /var/tmp/bench-rootfs-marker
			if start := strings.Index(cmd, `printf '%s' "`); start >= 0 {
				rest := cmd[start+len(`printf '%s' "`):]
				if end := strings.Index(rest, `"`); end >= 0 {
					m.mu.Lock()
					sbx.marker = rest[:end]
					m.mu.Unlock()
				}
			}
			writeExecdSSE(w, "")
		case strings.TrimSpace(cmd) == "cat /var/tmp/bench-rootfs-marker":
			m.mu.Lock()
			tok := sbx.marker
			m.mu.Unlock()
			writeExecdSSE(w, tok)
		case strings.Contains(cmd, "NPROC="):
			// Simulate typical Linux MemTotal (~96% of Firecracker allocated memory).
			memKiB := int64(float64(sbx.memMiB) * 0.96 * 1024)
			writeExecdSSE(w, fmt.Sprintf("NPROC=%d MEM_KB=%d\n", sbx.vcpus, memKiB))
		case strings.Contains(cmd, "ps -eo args"):
			m.mu.Lock()
			ep := strings.Join(sbx.entrypoint, " ")
			m.mu.Unlock()
			writeExecdSSE(w, "COMMAND\n/sbin/init\n"+ep+"\n")
		default:
			writeExecdSSE(w, "ok")
		}

	default:
		http.NotFound(w, r)
	}
}

func TestRootfsOnlyRestoreSuite_FullMatrixSourceStallAndCacheEvictOrder(t *testing.T) {
	mock, ts := newMockRootfsServer()
	defer ts.Close()
	mock.captureStallDuration = 60 * time.Millisecond

	var (
		evictCalls            int
		baseDeletedAtEvict    bool
		evictedNode           string
		isolateCalledWithNode string
	)

	s := NewRootfsOnlyRestoreSuite(
		WithRootfsPollInterval(5*time.Millisecond),
		WithRootfsCacheEvictor(func(_ context.Context, nodeName string) error {
			evictCalls++
			evictedNode = nodeName
			mock.mu.Lock()
			if base := mock.sandboxes["rootfs-sbx-1"]; base != nil {
				baseDeletedAtEvict = base.deleted
			}
			mock.mu.Unlock()
			return nil
		}),
		WithRootfsNodeIsolator(func(_ context.Context, nodeName string) (func(context.Context) error, error) {
			isolateCalledWithNode = nodeName
			mock.mu.Lock()
			mock.isolatedNode = nodeName
			mock.mu.Unlock()
			return func(_ context.Context) error {
				mock.mu.Lock()
				mock.isolatedNode = ""
				mock.mu.Unlock()
				return nil
			}, nil
		}),
	)

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack: profile.StackSpec{
			Template: profile.TemplateSpec{
				BaseImage: "python:3.12-bookworm",
				VCPUs:     2,
				MemoryMiB: 2048,
			},
		},
		Suites: profile.SuitesSpec{
			RootfsOnlyRestore: &profile.RootfsOnlyRestoreSuiteSpec{
				DirtyUpperMiB:       []int{0},
				FanoutConcurrencies: []int{2},
				CrossNode:           true,
				ResourceOverrides: []profile.ResourceOverrideSpec{
					{VCPUs: 1, MemoryMiB: 1024},
					{VCPUs: 4, MemoryMiB: 4096, Entrypoint: []string{"sleep", "3600"}},
				},
			},
		},
	}

	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-rootfs-1",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(ts.URL, "http://"),
			Protocol:   "http",
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status, "errors=%v samples=%+v", res.Errors, res.ErrorSamples)
	require.Equal(t, 0, res.FailedOps)
	require.Empty(t, res.ErrorsByOperation)

	// CacheEvictor must run AFTER baseID is deleted so overlaybd-ublkd restart never severs baseID,
	// and must pass empty nodeName to evict all runtime nodes in multi-node clusters.
	require.Equal(t, 1, evictCalls)
	require.Equal(t, "", evictedNode)
	require.True(t, baseDeletedAtEvict, "base sandbox must be deleted before CacheEvictor restarts overlaybd-ublkd")
	require.Equal(t, "node-a", isolateCalledWithNode)

	// Verify source stall telemetry & tier metadata.
	tiers, ok := res.Extra["dirty_upper_tiers"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, tiers, 1)
	require.Greater(t, tiers[0]["source_pause_stall_ms"].(float64), 20.0)
	require.Equal(t, true, tiers[0]["post_capture_source_ok"])
	require.Equal(t, true, tiers[0]["cold_cache_evicted"])
	require.Equal(t, true, tiers[0]["warm_marker_verified"])
	require.Equal(t, true, tiers[0]["cold_marker_verified"])

	// Cross-node verified.
	cn, ok := res.Extra["cross_node"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, cn["cross_node_ok"])
	require.Equal(t, "node-a", cn["before_node"])
	require.Equal(t, "node-b", cn["after_node"])

	// Overrides (scale-down + scale-up + entrypoint) verified.
	ovs, ok := res.Extra["resource_overrides"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, ovs, 2)
	require.Equal(t, true, ovs[0]["override_verified"])
	require.Equal(t, true, ovs[1]["override_verified"])
	require.Equal(t, true, ovs[1]["entrypoint_verified"])
}

func TestRootfsOnlyRestoreSuite_ScaleDownIgnoredFailsAndPopulatesErrorDiagnostics(t *testing.T) {
	mock, ts := newMockRootfsServer()
	defer ts.Close()
	mock.ignoreScaleDown = true

	evictFalse := false
	s := NewRootfsOnlyRestoreSuite(WithRootfsPollInterval(5 * time.Millisecond))
	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Stack: profile.StackSpec{
			Template: profile.TemplateSpec{
				BaseImage: "python:3.12-bookworm",
				VCPUs:     2,
				MemoryMiB: 2048,
			},
		},
		Suites: profile.SuitesSpec{
			RootfsOnlyRestore: &profile.RootfsOnlyRestoreSuiteSpec{
				DirtyUpperMiB:       []int{0},
				FanoutConcurrencies: []int{1},
				EvictCacheForCold:   &evictFalse,
				ResourceOverrides: []profile.ResourceOverrideSpec{
					{VCPUs: 1, MemoryMiB: 1024},
				},
			},
		},
	}

	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-scaledown-fail",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(ts.URL, "http://"),
			Protocol:   "http",
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.FailedOps)

	// F1: Scale-down ignored must fail instead of silently passing.
	ovs, ok := res.Extra["resource_overrides"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, ovs, 1)
	require.Equal(t, false, ovs[0]["ok"])
	require.Equal(t, false, ovs[0]["override_verified"])
	require.Contains(t, ovs[0]["error"], "110%")

	// F2: ErrorsByOperation and ErrorSamples must be populated with StderrHead.
	require.NotEmpty(t, res.ErrorsByOperation["rootfs_restore_override_1vcpu_1024mib"])
	require.Len(t, res.ErrorSamples, 1)
	require.Equal(t, "rootfs_restore_override_1vcpu_1024mib", res.ErrorSamples[0].Operation)
	require.Contains(t, res.ErrorSamples[0].StderrHead, "OSEP-0028 memory override mismatch")
}

func TestRootfsOnlyRestoreSuite_EvictCacheDisabledSkipsEvictor(t *testing.T) {
	_, ts := newMockRootfsServer()
	defer ts.Close()

	evictCalled := false
	evictFalse := false
	s := NewRootfsOnlyRestoreSuite(
		WithRootfsPollInterval(5*time.Millisecond),
		WithRootfsCacheEvictor(func(_ context.Context, _ string) error {
			evictCalled = true
			return nil
		}),
	)
	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Suites: profile.SuitesSpec{
			RootfsOnlyRestore: &profile.RootfsOnlyRestoreSuiteSpec{
				DirtyUpperMiB:       []int{0},
				FanoutConcurrencies: []int{1},
				EvictCacheForCold:   &evictFalse,
			},
		},
	}

	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-no-evict",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     strings.TrimPrefix(ts.URL, "http://"),
			Protocol:   "http",
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.False(t, evictCalled)

	tiers := res.Extra["dirty_upper_tiers"].([]map[string]any)
	require.Equal(t, false, tiers[0]["cold_cache_evicted"])
}
