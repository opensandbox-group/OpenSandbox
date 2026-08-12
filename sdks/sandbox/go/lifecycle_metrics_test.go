// Copyright 2026 Alibaba Group Holding Ltd.
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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReportSandboxCreateMetricPostsEvent(t *testing.T) {
	var got atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics/events" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var event sandboxCreateMetricsEvent
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if event.EventType != "sandbox.create" || !event.Success {
			t.Fatalf("unexpected event: %+v", event)
		}
		if gotUA := r.Header.Get("User-Agent"); !strings.HasPrefix(gotUA, "OpenSandbox-Go-SDK/") {
			t.Fatalf("unexpected User-Agent: %q", gotUA)
		}
		got.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{Domain: server.URL, Protocol: "http", APIKey: "k"}
	// Domain already has scheme via server.URL
	cfg.Domain = server.URL
	reportSandboxCreateMetric(cfg, "sbx", "python:3.12", 42, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metrics POST was not received")
}

func TestReportSandboxCreateMetricCarriesClientIPHeader(t *testing.T) {
	withStubbedClientIP(t, "10.9.8.7")

	var gotIP atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP.Store(r.Header.Get(clientIPHeader))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{Domain: server.URL, Protocol: "http", APIKey: "k"}
	reportSandboxCreateMetric(cfg, "sbx", "python:3.12", 42, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := gotIP.Load().(string); ok {
			require.Equal(t, "10.9.8.7", v)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metrics POST was not received")
}

func TestReportSandboxCreateMetricIgnoresFailure(t *testing.T) {
	cfg := ConnectionConfig{Domain: "http://127.0.0.1:1", Protocol: "http"}
	// Should not panic or block.
	reportSandboxCreateMetric(cfg, "", "img", 1, false)
	time.Sleep(50 * time.Millisecond)
}

func TestReportSandboxCreateMetricDisabledByConfig(t *testing.T) {
	var got atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{Domain: server.URL, DisableMetrics: true}
	reportSandboxCreateMetric(cfg, "sbx", "img", 1, true)
	time.Sleep(100 * time.Millisecond)
	if got.Load() {
		t.Fatal("expected metrics POST to be skipped when DisableMetrics is set")
	}
}

func TestReportSandboxCreateMetricDisabledByEnv(t *testing.T) {
	t.Setenv("OPENSANDBOX_DISABLE_METRICS", "1")

	var got atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := ConnectionConfig{Domain: server.URL}
	reportSandboxCreateMetric(cfg, "sbx", "img", 1, true)
	time.Sleep(100 * time.Millisecond)
	if got.Load() {
		t.Fatal("expected metrics POST to be skipped when OPENSANDBOX_DISABLE_METRICS=1")
	}
}
