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

package connectivity

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type staticSnapshotter struct {
	snapshot Snapshot
}

func (s staticSnapshotter) Snapshot(time.Time) Snapshot { return s.snapshot }

func TestReadinessHandlerIsShadowOnly(t *testing.T) {
	tests := []struct {
		name     string
		degraded bool
		body     string
	}{
		{name: "healthy", body: "OK"},
		{name: "degraded", degraded: true, body: "DEGRADED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler := NewReadinessHandler(staticSnapshotter{snapshot: Snapshot{Degraded: test.degraded}})
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status.ok/network-readiness", nil))
			if recorder.Code != http.StatusOK || recorder.Body.String() != test.body {
				t.Fatalf("response = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), test.body)
			}
			if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected response headers: %v", recorder.Header())
			}
		})
	}
}
