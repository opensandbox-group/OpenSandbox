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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

const disableMetricsEnv = "OPENSANDBOX_DISABLE_METRICS"

type sandboxCreateMetricsEvent struct {
	EventType        string `json:"eventType"`
	SandboxID        string `json:"sandboxId,omitempty"`
	Image            string `json:"image,omitempty"`
	CreateDurationMs int64  `json:"createDurationMs"`
	Success          bool   `json:"success"`
}

func metricsDisabled(cfg ConnectionConfig) bool {
	if cfg.DisableMetrics {
		return true
	}
	return strings.TrimSpace(os.Getenv(disableMetricsEnv)) == "1"
}

// reportSandboxCreateMetric best-effort posts create latency to the lifecycle server.
// Failures are ignored and the call never blocks CreateSandbox.
func reportSandboxCreateMetric(cfg ConnectionConfig, sandboxID, image string, durationMs int64, success bool) {
	if metricsDisabled(cfg) {
		return
	}

	payload := sandboxCreateMetricsEvent{
		EventType:        "sandbox.create",
		SandboxID:        sandboxID,
		Image:            image,
		CreateDurationMs: durationMs,
		Success:          success,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	go func() {
		defer func() { _ = recover() }()

		url := cfg.GetBaseURL() + "/" + APIVersion + "/metrics/events"
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 5 * time.Second}
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		apiKey := cfg.GetAPIKey()
		if apiKey != "" {
			req.Header.Set(cfg.GetAuthHeader(), apiKey)
		}
		for k, v := range cfg.Headers {
			if req.Header.Get(k) == "" {
				req.Header.Set(k, v)
			}
		}
		// This raw request bypasses NewClient (where the client IP is injected
		// into Client.headers), so attach it here too. req.Header.Get is
		// case-insensitive, so a user-supplied value above still wins.
		if ip := detectedClientIP(); ip != "" && req.Header.Get(clientIPHeader) == "" {
			req.Header.Set(clientIPHeader, ip)
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
	}()
}
