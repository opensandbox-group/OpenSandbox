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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeExecdSSE(w http.ResponseWriter, stdout string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if stdout != "" {
		b, _ := json.Marshal(map[string]any{"type": "stdout", "text": stdout})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprintf(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
}

type mockSoakServer struct {
	mu           sync.Mutex
	serverURL    string
	seq          atomic.Int64
	sandboxes    map[string]opensandbox.SandboxState
	denyAbove    int
	failList     bool
	keepResidual bool
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
		if m.failList {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "LIST_ERROR", "message": "injected list failure"})
			return
		}
		m.mu.Lock()
		items := make([]opensandbox.SandboxInfo, 0, len(m.sandboxes))
		for id, st := range m.sandboxes {
			items = append(items, opensandbox.SandboxInfo{
				ID:     id,
				Status: opensandbox.SandboxStatus{State: st},
			})
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, opensandbox.ListSandboxesResponse{Items: items})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/")
		writeJSON(w, http.StatusOK, opensandbox.Endpoint{
			Endpoint: fmt.Sprintf("%s/execd/%s", m.serverURL, parts[0]),
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
		sbxID := strings.TrimPrefix(path, "/v1/sandboxes/")
		m.mu.Lock()
		if !m.keepResidual {
			delete(m.sandboxes, sbxID)
		}
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/ping"):
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/execd/") && strings.HasSuffix(path, "/command"):
		writeExecdSSE(w, "soak-ok")

	default:
		http.NotFound(w, r)
	}
}
