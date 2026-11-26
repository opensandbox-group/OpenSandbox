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
)

type mockRootfsServer struct {
	mu                 sync.Mutex
	serverURL          string
	sbxSeq             atomic.Int64
	snapSeq            atomic.Int64
	isolatedNode       string
	sandboxes          map[string]*mockRootfsSandbox
	snapshots          map[string]*mockRootfsSnapshot
	overrideSeen       []opensandbox.ResourceLimits
	snapshotModesSeen  []opensandbox.SnapshotMode
	deletedSandboxes   int
	deletedSnapshots   int
	ignoreIsolation    bool
}

type mockRootfsSandbox struct {
	id         string
	node       string
	snapshotID string
	marker     string
	state      opensandbox.SandboxState
	limits     opensandbox.ResourceLimits
}

type mockRootfsSnapshot struct {
	id        string
	sandboxID string
	mode      opensandbox.SnapshotMode
	marker    string
	polls     int
}

var markerWriteRe = regexp.MustCompile(`printf '%s' "([^"]+)" > /tmp/bench-rootfs-marker`)

func newMockRootfsServer() (*mockRootfsServer, *httptest.Server) {
	m := &mockRootfsServer{
		sandboxes: make(map[string]*mockRootfsSandbox),
		snapshots: make(map[string]*mockRootfsSnapshot),
	}
	ts := httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.serverURL = ts.URL
	return m, ts
}

func (m *mockRootfsServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/sandboxes":
		var req opensandbox.CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := fmt.Sprintf("sbx-%d", m.sbxSeq.Add(1))
		m.mu.Lock()
		node := "node-1"
		if !m.ignoreIsolation && m.isolatedNode == "node-1" {
			node = "node-2"
		}
		marker := ""
		if req.SnapshotID != "" {
			if snap, ok := m.snapshots[req.SnapshotID]; ok {
				marker = snap.marker
			}
			if len(req.ResourceLimits) > 0 {
				cp := make(opensandbox.ResourceLimits, len(req.ResourceLimits))
				for k, v := range req.ResourceLimits {
					cp[k] = v
				}
				m.overrideSeen = append(m.overrideSeen, cp)
			}
		}
		sbx := &mockRootfsSandbox{
			id:         id,
			node:       node,
			snapshotID: req.SnapshotID,
			marker:     marker,
			state:      opensandbox.StateRunning,
			limits:     req.ResourceLimits,
		}
		m.sandboxes[id] = sbx
		m.mu.Unlock()

		writeJSON(w, http.StatusAccepted, opensandbox.SandboxInfo{
			ID:         id,
			SnapshotID: req.SnapshotID,
			Status:     opensandbox.SandboxStatus{State: opensandbox.StateRunning},
			Metadata:   map[string]string{"node": node},
			CreatedAt:  time.Now().UTC(),
		})

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/sandboxes/") && strings.HasSuffix(path, "/snapshots"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/sandboxes/"), "/snapshots")
		var req opensandbox.CreateSnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		snapID := fmt.Sprintf("snap-%d", m.snapSeq.Add(1))

		m.mu.Lock()
		sbx := m.sandboxes[sbxID]
		marker := ""
		if sbx != nil {
			marker = sbx.marker
		}
		m.snapshotModesSeen = append(m.snapshotModesSeen, req.Mode)
		m.snapshots[snapID] = &mockRootfsSnapshot{
			id:        snapID,
			sandboxID: sbxID,
			mode:      req.Mode,
			marker:    marker,
		}
		m.mu.Unlock()

		writeJSON(w, http.StatusAccepted, opensandbox.SnapshotInfo{
			ID:        snapID,
			SandboxID: sbxID,
			Name:      req.Name,
			Mode:      req.Mode,
			Status:    opensandbox.SnapshotStatus{State: opensandbox.SnapshotStateCreating},
			CreatedAt: time.Now().UTC(),
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/snapshots/"):
		snapID := strings.TrimPrefix(path, "/v1/snapshots/")
		m.mu.Lock()
		snap, ok := m.snapshots[snapID]
		if !ok {
			m.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		snap.polls++
		state := opensandbox.SnapshotStateCreating
		if snap.polls >= 1 {
			state = opensandbox.SnapshotStateReady
		}
		info := opensandbox.SnapshotInfo{
			ID:        snap.id,
			SandboxID: snap.sandboxID,
			Mode:      snap.mode,
			Status:    opensandbox.SnapshotStatus{State: state},
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, info)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/snapshots/"):
		snapID := strings.TrimPrefix(path, "/v1/snapshots/")
		m.mu.Lock()
		delete(m.snapshots, snapID)
		m.deletedSnapshots++
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/")
		sbxID := parts[0]
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: fmt.Sprintf("%s/execd/%s", m.serverURL, sbxID),
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		sbx, ok := m.sandboxes[sbxID]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, opensandbox.SandboxInfo{
			ID:       sbx.id,
			Status:   opensandbox.SandboxStatus{State: sbx.state},
			Metadata: map[string]string{"node": sbx.node},
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		delete(m.sandboxes, sbxID)
		m.deletedSandboxes++
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/ping"):
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/command"):
		sbxID := strings.TrimSuffix(strings.TrimPrefix(path, "/execd/"), "/command")
		var req opensandbox.RunCommandRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		var out string
		m.mu.Lock()
		sbx := m.sandboxes[sbxID]
		if sbx != nil {
			if matches := markerWriteRe.FindStringSubmatch(req.Command); len(matches) == 2 {
				sbx.marker = matches[1]
				out = "ok"
			} else if strings.Contains(req.Command, "cat /tmp/bench-rootfs-marker") {
				out = sbx.marker
			}
		}
		m.mu.Unlock()

		writeExecdSSE(w, out)

	default:
		http.NotFound(w, r)
	}
}

func writeExecdSSE(w http.ResponseWriter, stdout string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{
		"type": "stdout",
		"text": stdout,
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"execution_complete\"}\n\n")
}

func TestRootfsOnlyRestoreSuite_FullMatrix(t *testing.T) {
	mock, ts := newMockRootfsServer()
	defer ts.Close()

	var isolatedCalls atomic.Int32
	var restoredCalls atomic.Int32
	isolator := func(_ context.Context, nodeName string) (func(context.Context) error, error) {
		isolatedCalls.Add(1)
		mock.mu.Lock()
		mock.isolatedNode = nodeName
		mock.mu.Unlock()
		return func(_ context.Context) error {
			restoredCalls.Add(1)
			mock.mu.Lock()
			mock.isolatedNode = ""
			mock.mu.Unlock()
			return nil
		}, nil
	}

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
			RootfsOnlyRestore: &profile.RootfsOnlyRestoreSuiteSpec{
				DirtyUpperMiB:       []int{0, 16},
				FanoutConcurrencies: []int{1, 4},
				CrossNode:           true,
				ResourceOverrides: []profile.ResourceOverrideSpec{
					{VCPUs: 2, MemoryMiB: 4096},
				},
			},
		},
	}

	s := NewRootfsOnlyRestoreSuite(
		WithRootfsNodeIsolator(isolator),
		WithRootfsPollInterval(2*time.Millisecond),
	)
	require.Equal(t, "rootfs-only-restore", s.Name())
	require.Equal(t, []string{profile.RuntimeFastSandboxOverlayBD}, s.SupportedRuntimes())

	res, samples, err := s.Run(context.Background(), RunContext{
		RunID:   "test-rootfs-1",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", res.Status)
	require.Zero(t, res.FailedOps)
	require.Greater(t, res.SucceededOps, 0)
	require.Equal(t, res.TotalOps, len(samples))

	// Required latency distributions.
	for _, key := range []string{
		"t_api_ack",
		"t_first_ping",
		"t_first_exec",
		"guest_boot_to_first_ping_gap",
		"snapshot_create",
		"cold_materialize_first_ping",
		"warm_materialize_first_ping",
		"cross_node_first_ping",
		"override_first_ping",
	} {
		dist, ok := res.Latencies[key]
		require.True(t, ok, "missing latency distribution %q", key)
		require.Greater(t, dist.Count, 0, "empty latency distribution %q", key)
	}

	// Verify RootfsOnly snapshot mode was requested for every dirty tier.
	require.Equal(t, []opensandbox.SnapshotMode{
		opensandbox.SnapshotModeRootfsOnly,
		opensandbox.SnapshotModeRootfsOnly,
	}, mock.snapshotModesSeen)

	// Verify cross-node isolation hook ran and asserted before_node != after_node.
	require.Equal(t, int32(1), isolatedCalls.Load())
	require.Equal(t, int32(1), restoredCalls.Load())
	cn, ok := res.Extra["cross_node"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "node-1", cn["before_node"])
	require.Equal(t, "node-2", cn["after_node"])
	require.Equal(t, true, cn["cross_node_ok"])

	// Verify OSEP-0028 resource overrides were sent.
	require.Len(t, mock.overrideSeen, 1)
	require.Equal(t, "2", mock.overrideSeen[0]["cpu"])
	require.Equal(t, "4096Mi", mock.overrideSeen[0]["memory"])

	// Verify cleanup left zero sandboxes and snapshots behind.
	mock.mu.Lock()
	require.Empty(t, mock.sandboxes)
	require.Empty(t, mock.snapshots)
	mock.mu.Unlock()
}

func TestRootfsOnlyRestoreSuite_CrossNodeViolationFails(t *testing.T) {
	mock, ts := newMockRootfsServer()
	mock.ignoreIsolation = true // stays on node-1 even when isolated
	defer ts.Close()

	isolator := func(_ context.Context, _ string) (func(context.Context) error, error) {
		return func(_ context.Context) error { return nil }, nil
	}

	prof := &profile.BenchmarkProfile{
		Runtime: profile.RuntimeFastSandboxOverlayBD,
		Suites: profile.SuitesSpec{
			RootfsOnlyRestore: &profile.RootfsOnlyRestoreSuiteSpec{
				DirtyUpperMiB:       []int{0},
				FanoutConcurrencies: []int{1},
				CrossNode:           true,
			},
		},
	}

	s := NewRootfsOnlyRestoreSuite(
		WithRootfsNodeIsolator(isolator),
		WithRootfsPollInterval(2*time.Millisecond),
	)
	res, _, err := s.Run(context.Background(), RunContext{
		RunID:   "test-rootfs-cn-fail",
		Profile: prof,
		SDKConfig: opensandbox.ConnectionConfig{
			Domain:     ts.URL,
			HTTPClient: ts.Client(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "failed", res.Status)
	require.Equal(t, 1, res.FailedOps)
	cn := res.Extra["cross_node"].(map[string]any)
	require.Equal(t, false, cn["cross_node_ok"])
}
