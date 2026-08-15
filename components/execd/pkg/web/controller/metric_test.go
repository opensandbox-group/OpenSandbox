// Copyright 2025 Alibaba Group Holding Ltd.
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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func setupMetricController(method, path string) (*MetricController, *httptest.ResponseRecorder) {
	ctx, w := newTestContext(method, path, nil)
	ctrl := NewMetricController(ctx)
	return ctrl, w
}

// TestReadMetrics exercises readMetrics end-to-end.
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
	assert.LessOrEqual(t, metrics.MemUsedMiB, metrics.MemTotalMiB) // Used memory should not exceed total

	currentTime := time.Now().UnixMilli()
	oneMinuteAgo := currentTime - 60*1000
	assert.GreaterOrEqual(t, metrics.Timestamp, oneMinuteAgo) // Should be within the last minute
	assert.LessOrEqual(t, metrics.Timestamp, currentTime)     // Should not be in the future
}

// TestGetMetricsEndpoint covers the happy path.
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

// TestWatchMetricsHeaders verifies SSE header defaults.
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

// TestMetricSerialization ensures metrics marshal and unmarshal cleanly.
func TestMetricSerialization(t *testing.T) {
	metrics := &model.Metrics{
		CpuCount:    4,
		CpuUsedPct:  25.5,
		MemTotalMiB: 8192,
		MemUsedMiB:  4096,
		Timestamp:   time.Now().UnixMilli(),
	}

	data, err := json.Marshal(metrics)
	assert.NoError(t, err)

	var decodedMetrics model.Metrics
	err = json.Unmarshal(data, &decodedMetrics)
	assert.NoError(t, err)
	assert.Equal(t, metrics.CpuCount, decodedMetrics.CpuCount)
	assert.Equal(t, metrics.CpuUsedPct, decodedMetrics.CpuUsedPct)
	assert.Equal(t, metrics.MemTotalMiB, decodedMetrics.MemTotalMiB)
	assert.Equal(t, metrics.MemUsedMiB, decodedMetrics.MemUsedMiB)
	assert.Equal(t, metrics.Timestamp, decodedMetrics.Timestamp)

	errorMsg := map[string]string{"error": "test error"}
	errorData, err := json.Marshal(errorMsg)
	assert.NoError(t, err)

	var decodedError map[string]string
	err = json.Unmarshal(errorData, &decodedError)
	assert.NoError(t, err)
	assert.Equal(t, "test error", decodedError["error"])
}
