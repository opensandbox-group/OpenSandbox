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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitForRunning_TimesOutWithoutContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SandboxInfo{
			ID:     "sbx-timeout",
			Status: SandboxStatus{State: StatePending},
		})
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-timeout",
		lifecycle: NewLifecycleClient(srv.URL, ""),
	}

	err := sb.waitForRunning(context.Background(), 100*time.Millisecond)
	require.Error(t, err)

	var timeoutErr *SandboxRunningTimeoutError
	assert.ErrorAs(t, err, &timeoutErr)
}

func TestWaitForRunning_SucceedsWhenRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SandboxInfo{
			ID:     "sbx-running",
			Status: SandboxStatus{State: StateRunning},
		})
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-running",
		lifecycle: NewLifecycleClient(srv.URL, ""),
	}

	require.NoError(t, sb.waitForRunning(context.Background(), 100*time.Millisecond))
}

func TestConnectSandbox_MultipleReadyOptionsReturnsError(t *testing.T) {
	_, err := ConnectSandbox(
		context.Background(),
		ConnectionConfig{},
		"sbx-1",
		ReadyOptions{Timeout: time.Second},
		ReadyOptions{Timeout: 2 * time.Second},
	)
	require.Error(t, err)

	var argErr *InvalidArgumentError
	require.ErrorAs(t, err, &argErr)
	require.Equal(t, "opts", argErr.Field)
}

func TestWaitUntilReady_RespectsContextCancellationDuringBackoff(t *testing.T) {
	sb := &Sandbox{id: "sbx-ready-cancel"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sb.WaitUntilReady(ctx, ReadyOptions{
		Timeout:         5 * time.Second,
		PollingInterval: 2 * time.Second,
		HealthCheck: func(context.Context, *Sandbox) (bool, error) {
			return false, nil
		},
	})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.LessOrEqual(t, elapsed, 500*time.Millisecond, "WaitUntilReady should stop quickly on ctx cancel")
}
