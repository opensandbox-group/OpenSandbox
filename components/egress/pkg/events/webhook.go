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

package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const (
	webhookSource         = "opensandbox-egress"
	defaultWebhookTimeout = 5 * time.Second
	defaultWebhookRetries = 3
	defaultWebhookBackoff = 1 * time.Second
)

type WebhookSubscriber struct {
	url        string
	client     *http.Client
	timeout    time.Duration
	maxRetries int
	backoff    time.Duration
	sandboxID  string
}

type webhookPayload struct {
	Hostname  string `json:"hostname"`
	Timestamp string `json:"timestamp"`
	Source    string `json:"source"`
	SandboxID string `json:"sandboxId"`
}

// NewWebhookSubscriber posts JSON to url with small retry/backoff (default queue consumer).
func NewWebhookSubscriber(url string) *WebhookSubscriber {
	if url == "" {
		return nil
	}
	return &WebhookSubscriber{
		url:        url,
		client:     &http.Client{},
		timeout:    defaultWebhookTimeout,
		maxRetries: defaultWebhookRetries,
		backoff:    defaultWebhookBackoff,
		sandboxID:  os.Getenv(constants.EnvSandboxID),
	}
}

func (w *WebhookSubscriber) HandleBlocked(ctx context.Context, ev BlockedEvent) {
	payload := webhookPayload{
		Hostname:  ev.Hostname,
		Timestamp: ev.Timestamp.UTC().Format(time.RFC3339),
		Source:    webhookSource,
		SandboxID: w.sandboxID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Warnf("[webhook] failed to marshal payload for hostname %s: %v", ev.Hostname, err)
		return
	}

	var lastErr error
	for attempt := 0; attempt <= w.maxRetries; attempt++ {
		reqCtx := ctx
		cancel := func() {}
		if w.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, w.timeout)
		}

		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.url, bytes.NewReader(body))
		if err != nil {
			cancel()
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := w.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				cancel()
				return
			}
			if resp.StatusCode < 500 {
				cancel()
				log.Warnf("[webhook] non-retriable status %d for hostname %s", resp.StatusCode, payload.Hostname)
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}

		cancel()
		lastErr = err
		if attempt < w.maxRetries {
			time.Sleep(w.backoff * time.Duration(1<<attempt))
		}
	}

	if lastErr != nil {
		log.Warnf("[webhook] failed to notify hostname %s after retries: %v", payload.Hostname, lastErr)
	}
}
