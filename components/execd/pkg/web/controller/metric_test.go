// Copyright 2025 The OpenSandbox Authors
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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func setupMetricController(method, path string) (*MetricController, *httptest.ResponseRecorder) {
	ctx, w := newTestContext(method, path, nil)
	ctrl := NewMetricController(ctx)
	return ctrl, w
}

func TestReadMetrics(t *testing.T) {
	ctrl := &MetricController{}

	metrics, err := ctrl.readMetrics()

	assert.NoError(t, err)
	assert.NotNil(t, metrics)

	assert.Greater(t, metrics.CpuCount, 0.0)

	assert.GreaterOrEqual(t, metrics.CpuUsedPct, 0.0)
	assert.Less(t, metrics.CpuUsedPct, 100.1) // CPU usage should be under 100% with small float tolerance

	assert.Greater(t, metrics.MemTotalMiB, 0.0)
	assert.GreaterOrEqual(t, metrics.MemUsedMiB, 0.0)
	assert.LessOrEqual(t, metrics.MemUsedMiB, metrics.MemTotalMiB)

	currentTime := time.Now().UnixMilli()
	oneMinuteAgo := currentTime - 60*1000
	assert.GreaterOrEqual(t, metrics.Timestamp, oneMinuteAgo)
	assert.LessOrEqual(t, metrics.Timestamp, currentTime)
}

func TestGetMetricsEndpoint(t *testing.T) {
	ctrl, w := setupMetricController("GET", "/api/metrics")

	ctrl.GetMetrics()

	assert.Equal(t, http.StatusOK, w.Code)

	var metrics model.Metrics
	err := json.Unmarshal(w.Body.Bytes(), &metrics)
	assert.NoError(t, err)

	assert.Greater(t, metrics.CpuCount, 0.0)
	assert.GreaterOrEqual(t, metrics.CpuUsedPct, 0.0)
	assert.Greater(t, metrics.MemTotalMiB, 0.0)
	assert.GreaterOrEqual(t, metrics.MemUsedMiB, 0.0)
	assert.NotZero(t, metrics.Timestamp)
}

func TestWatchMetricsHeaders(t *testing.T) {
	ctrl, w := setupMetricController("GET", "/api/watch-metrics")

	ctrl.setupSSEResponse()

	contentType := w.Header().Get("Content-Type")
	assert.Equal(t, "text/event-stream", contentType)

	cacheControl := w.Header().Get("Cache-Control")
	assert.Equal(t, "no-cache", cacheControl)

	connection := w.Header().Get("Connection")
	assert.Equal(t, "keep-alive", connection)

	buffering := w.Header().Get("X-Accel-Buffering")
	assert.Equal(t, "no", buffering)
}

func TestMetricSerialization(t *testing.T) {
	metrics := &model.Metrics{
		CpuCount:    4,
		CpuUsedPct:  25.5,
		MemTotalMiB: 8192,
		MemUsedMiB:  4096,
		Timestamp:   time.Now().UnixMilli(),
	}

	// Serialize under the JSON keys defined by the model tags so that
	// external consumers (SDKs, dashboards) see a stable wire format.
	data, err := json.Marshal(metrics)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, float64(4), decoded["cpu_count"])
	assert.Equal(t, 25.5, decoded["cpu_used_pct"])
	assert.Equal(t, float64(8192), decoded["mem_total_mib"])
	assert.Equal(t, float64(4096), decoded["mem_used_mib"])
	assert.NotEmpty(t, decoded["timestamp"])
}

// The spec promises an update every second. Sampling CPU usage already takes
// one second, so the stream must not wait another second on top of it.
func TestWatchMetricsSendsAnUpdateEverySecond(t *testing.T) {
	ctrl, w := setupMetricController("GET", "/metrics/watch")
	reqCtx, cancel := context.WithCancel(context.Background())
	ctrl.ctx.Request = ctrl.ctx.Request.WithContext(reqCtx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.WatchMetrics()
	}()

	time.Sleep(2500 * time.Millisecond)
	cancel()
	cancelled := time.Now()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchMetrics did not return after the client disconnected")
	}
	assert.Less(t, time.Since(cancelled), 2500*time.Millisecond, "a disconnect should end the stream after the sample in progress")

	frames := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m model.Metrics
		require.NoError(t, json.Unmarshal([]byte(line), &m), "frame %q", line)
		frames++
	}
	assert.GreaterOrEqual(t, frames, 2, "2.5s of watching should yield an update per second")
}
