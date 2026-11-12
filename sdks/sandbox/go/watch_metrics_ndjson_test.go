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

package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// execd's /metrics/watch writes one JSON object per line, with no blank line
// between objects, and keeps the stream open until the client disconnects.
func TestWatchMetrics_DeliversNewlineDelimitedSnapshots(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 1; i <= 2; i++ {
			msg, _ := json.Marshal(Metrics{CPUCount: float64(i), Timestamp: int64(i)})
			_, _ = w.Write(append(msg, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errDone := errors.New("got two snapshots")
	var got []Metrics
	err := client.WatchMetrics(ctx, func(event StreamEvent) error {
		var m Metrics
		if err := json.Unmarshal([]byte(event.Data), &m); err != nil {
			return err
		}
		got = append(got, m)
		if len(got) == 2 {
			return errDone
		}
		return nil
	})

	if !errors.Is(err, errDone) {
		t.Fatalf("WatchMetrics returned %v after %d snapshots, want 2 snapshots", err, len(got))
	}
	if got[0].CPUCount != 1 || got[1].CPUCount != 2 {
		t.Fatalf("snapshots = %+v, want cpu_count 1 then 2", got)
	}
}
