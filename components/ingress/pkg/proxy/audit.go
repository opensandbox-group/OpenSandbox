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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
)

// AuditEvent describes a single sandbox access request.
type AuditEvent struct {
	// SandboxID is the id of the sandbox instance being accessed.
	SandboxID string `json:"sandbox_id"`

	// URI is the request URI of the incoming request.
	URI string `json:"uri"`

	// Method is the HTTP method of the incoming request.
	Method string `json:"method"`

	// Target is the resolved sandbox endpoint (host:port) the request is forwarded to.
	Target string `json:"target"`

	// RequestTime is the time the request was received by the ingress.
	RequestTime time.Time `json:"request_time"`
}

const (
	defaultAuditQueueSize = 1024
	defaultAuditTimeout   = 3 * time.Second
)

// AuditReporter asynchronously delivers audit events to a webhook address.
// Events are queued in a buffered channel and posted by a background worker,
// so Report never blocks the request path; events are dropped when the
// queue is full or the reporter has been stopped.
type AuditReporter struct {
	url    string
	client *http.Client
	events chan AuditEvent
	quit   chan struct{}
	done   chan struct{}
	stop   sync.Once
}

// NewAuditReporter creates an AuditReporter posting to the given url.
// queueSize bounds the number of pending events; timeout bounds each post.
func NewAuditReporter(url string, queueSize int, timeout time.Duration) *AuditReporter {
	if queueSize <= 0 {
		queueSize = defaultAuditQueueSize
	}
	if timeout <= 0 {
		timeout = defaultAuditTimeout
	}

	return &AuditReporter{
		url:    url,
		client: &http.Client{Timeout: timeout},
		events: make(chan AuditEvent, queueSize),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start launches the background worker that delivers queued events.
// The worker stops when ctx is cancelled or Stop is called.
func (r *AuditReporter) Start(ctx context.Context) {
	go func() {
		defer close(r.done)

		for {
			select {
			case <-ctx.Done():
				return
			case <-r.quit:
				return
			case event := <-r.events:
				if err := r.send(ctx, event); err != nil {
					Logger.With(
						slogger.Field{Key: "error", Value: err},
						slogger.Field{Key: "target_url", Value: r.url},
					).Errorf("AuditReporter: failed to deliver audit event")
				}
			}
		}
	}()
}

// Stop shuts down the reporter and waits for the worker to exit.
// Events still queued are dropped.
func (r *AuditReporter) Stop() {
	r.stop.Do(func() {
		close(r.quit)
		<-r.done
	})
}

// Report queues an event for asynchronous delivery. It never blocks:
// when the queue is full or the reporter is stopped, the event is dropped.
func (r *AuditReporter) Report(event AuditEvent) {
	select {
	case <-r.quit:
		return
	case r.events <- event:
	default:
		Logger.Warnf("AuditReporter: event queue is full, dropping audit event")
	}
}

func (r *AuditReporter) send(ctx context.Context, event AuditEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("audit webhook returned status %d", resp.StatusCode)
	}
	return nil
}
