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

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/stretchr/testify/assert"
)

func Test_AuditReporter(t *testing.T) {
	events := make(chan AuditEvent, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.Nil(t, err)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var event AuditEvent
		assert.Nil(t, json.Unmarshal(body, &event))
		events <- event
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	Logger = slogger.MustNew(slogger.Config{Level: "debug"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reporter := NewAuditReporter(server.URL, 8, time.Second)
	reporter.Start(ctx)
	defer reporter.Stop()

	reporter.Report(AuditEvent{
		SandboxID:   "test-sandbox",
		URI:         "/api/users",
		Method:      http.MethodGet,
		Target:      "10.0.0.1:8080",
		RequestTime: time.Now(),
	})

	select {
	case event := <-events:
		assert.Equal(t, "test-sandbox", event.SandboxID)
		assert.Equal(t, "/api/users", event.URI)
		assert.Equal(t, http.MethodGet, event.Method)
		assert.Equal(t, "10.0.0.1:8080", event.Target)
		assert.False(t, event.RequestTime.IsZero())
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}

func Test_AuditReporterDropsWhenQueueFull(t *testing.T) {
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})

	// Reporter started without a reachable webhook: the worker stays busy
	// so the queue fills up and Report drops instead of blocking.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reporter := NewAuditReporter(server.URL, 1, time.Second)
	reporter.Start(context.Background())
	defer reporter.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 32; i++ {
			reporter.Report(AuditEvent{SandboxID: fmt.Sprintf("sandbox-%d", i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Report blocked on a full queue")
	}
}

func Test_AuditReporterNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	Logger = slogger.MustNew(slogger.Config{Level: "debug"})

	reporter := NewAuditReporter(server.URL, 8, time.Second)
	reporter.Start(context.Background())
	reporter.Report(AuditEvent{SandboxID: "test-sandbox"})

	// The reporter logs the failure and keeps serving.
	time.Sleep(200 * time.Millisecond)
	reporter.Stop()
}

func Test_ProxyReportsAuditEvent(t *testing.T) {
	auditEvents := make(chan AuditEvent, 8)
	auditServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.Nil(t, err)

		var event AuditEvent
		assert.Nil(t, json.Unmarshal(body, &event))
		auditEvents <- event
		w.WriteHeader(http.StatusOK)
	}))
	defer auditServer.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	backendPort := backend.URL[len("http://127.0.0.1:"):]
	port, err := strconv.Atoi(backendPort)
	assert.Nil(t, err)

	provider := &mockProvider{
		endpoints: map[string]string{
			"test-sandbox": "127.0.0.1",
		},
	}

	Logger = slogger.MustNew(slogger.Config{Level: "debug"})

	ctx := context.Background()
	reporter := NewAuditReporter(auditServer.URL, 8, time.Second)
	reporter.Start(ctx)
	defer reporter.Stop()

	proxy := NewProxy(ctx, provider, ModeHeader, nil, nil).EnableAudit(reporter)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	ingressPort, err := findAvailablePort()
	assert.Nil(t, err)

	go func() {
		_ = http.ListenAndServe(":"+strconv.Itoa(ingressPort), mux)
	}()
	time.Sleep(2 * time.Second)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/hello", ingressPort), nil)
	assert.Nil(t, err)
	request.Header.Set(SandboxIngress, fmt.Sprintf("test-sandbox-%d", port))

	response, err := http.DefaultClient.Do(request)
	assert.Nil(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)

	select {
	case event := <-auditEvents:
		assert.Equal(t, "test-sandbox", event.SandboxID)
		assert.Equal(t, "/hello", event.URI)
		assert.Equal(t, http.MethodGet, event.Method)
		assert.Equal(t, fmt.Sprintf("127.0.0.1:%d", port), event.Target)
		assert.False(t, event.RequestTime.IsZero())
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}
