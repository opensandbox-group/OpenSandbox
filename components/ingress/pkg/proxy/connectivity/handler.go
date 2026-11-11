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
	"time"
)

// Snapshotter supplies the current fixed-window connectivity assessment.
type Snapshotter interface {
	Snapshot(time.Time) Snapshot
}

// NewReadinessHandler creates a shadow-only readiness endpoint. It reports an
// observed degradation in the body but intentionally never removes traffic.
func NewReadinessHandler(tracker Snapshotter) http.Handler {
	return newReadinessHandler(tracker, time.Now)
}

func newReadinessHandler(tracker Snapshotter, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		snapshot := tracker.Snapshot(now())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if snapshot.Degraded {
			_, _ = w.Write([]byte("DEGRADED"))
			return
		}
		_, _ = w.Write([]byte("OK"))
	})
}
