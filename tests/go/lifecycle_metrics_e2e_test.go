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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestLifecycleMetrics_EndpointAcceptsEvent(t *testing.T) {
	cfg := getConnectionConfig(t)
	url := cfg.GetBaseURL() + "/" + opensandbox.APIVersion + "/metrics/events"
	payload := map[string]any{
		"eventType":        "sandbox.create",
		"sandboxId":        "e2e-metrics-direct-go",
		"image":            getSandboxImage(),
		"createDurationMs": 42,
		"success":          true,
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/e2e")
	req.Header.Set(cfg.GetAuthHeader(), cfg.GetAPIKey())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestSandbox_CreateReportsLifecycleMetrics(t *testing.T) {
	cfg := getConnectionConfig(t)

	var (
		mu        sync.Mutex
		captured  []map[string]any
		userAgent string
	)
	base := http.DefaultTransport
	cfg.HTTPClient = &http.Client{
		Timeout: 3 * time.Minute,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/metrics/events") {
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				req.Body = io.NopCloser(bytes.NewReader(raw))
				var event map[string]any
				if err := json.Unmarshal(raw, &event); err == nil {
					mu.Lock()
					captured = append(captured, event)
					userAgent = req.Header.Get("User-Agent")
					mu.Unlock()
				}
			}
			return base.RoundTrip(req)
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
		Image:      getSandboxImage(),
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		Env:        map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s"},
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    "500m",
			"memory": "256Mi",
		},
		Metadata: map[string]string{"tag": "e2e-lifecycle-metrics"},
	})
	require.NoError(t, err)
	defer func() {
		_ = sb.Kill(context.Background())
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(captured)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(captured), 1)
	event := captured[0]
	require.Equal(t, "sandbox.create", event["eventType"])
	require.Equal(t, true, event["success"])
	require.Equal(t, sb.ID(), event["sandboxId"])
	_, hasLang := event["sdkLanguage"]
	_, hasVer := event["sdkVersion"]
	require.False(t, hasLang)
	require.False(t, hasVer)
	require.True(t, strings.HasPrefix(userAgent, "OpenSandbox-Go-SDK/"))
	duration, ok := event["createDurationMs"].(float64)
	require.True(t, ok)
	require.Greater(t, duration, float64(0))
	t.Logf("sandbox.create metrics createDurationMs=%.0fms sandboxId=%s", duration, sb.ID())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
