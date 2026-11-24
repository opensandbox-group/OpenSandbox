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
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestWaitForRunningDeadlineDuringStatusRequest(t *testing.T) {
	for _, parentDeadline := range []bool{false, true} {
		name := "running budget"
		if parentDeadline {
			name = "parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			budget := 30 * time.Millisecond
			if parentDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
				budget = time.Second
			}
			requests := 0
			client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			sb := &Sandbox{id: "pending", lifecycle: NewLifecycleClient("http://sandbox.test", "", WithHTTPClient(client))}

			err := sb.waitForRunning(ctx, budget)

			var timeoutErr *SandboxRunningTimeoutError
			if !errors.As(err, &timeoutErr) {
				t.Fatalf("error = %v, want SandboxRunningTimeoutError", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) || timeoutErr.SandboxID != "pending" {
				t.Fatalf("timeout lost its cause or sandbox ID: %#v", timeoutErr)
			}
			if requests != 1 {
				t.Fatalf("status requests = %d, want 1", requests)
			}
		})
	}
}

func TestWaitForRunningCancellationDuringStatusRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	sb := &Sandbox{id: "pending", lifecycle: NewLifecycleClient("http://sandbox.test", "", WithHTTPClient(client))}

	err := sb.waitForRunning(ctx, time.Second)

	var timeoutErr *SandboxRunningTimeoutError
	if !errors.Is(err, context.Canceled) || errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want caller cancellation without a running timeout", err)
	}
}

func TestWaitForRunningRequestErrorBeforeDeadline(t *testing.T) {
	client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	sb := &Sandbox{id: "pending", lifecycle: NewLifecycleClient("http://sandbox.test", "", WithHTTPClient(client))}

	err := sb.waitForRunning(context.Background(), time.Minute)

	var timeoutErr *SandboxRunningTimeoutError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, transport timeout must not become a running-budget timeout", err)
	}
}
