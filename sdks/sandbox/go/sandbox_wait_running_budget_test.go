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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Use an immediate in-memory response so these budget tests cannot race
// a TCP connection or response body read against their short deadlines.
func pendingRunningSandbox() *Sandbox {
	client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"pending","status":{"state":"Pending"}}`))}, nil
	})}
	return &Sandbox{id: "pending", lifecycle: NewLifecycleClient("http://sandbox.test", "", WithHTTPClient(client))}
}

func TestWaitForRunningBoundsLongerParentDeadline(t *testing.T) {
	sb := pendingRunningSandbox()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := sb.waitForRunning(ctx, 30*time.Millisecond)
	var timeoutErr *SandboxRunningTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want running timeout", err)
	}
	if elapsed := time.Since(started); elapsed >= 300*time.Millisecond {
		t.Fatalf("30ms running budget used %v with 500ms parent deadline", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatalf("running timeout exhausted parent context: %v", ctx.Err())
	}
}

func TestWaitForRunningKeepsEarlierParentDeadline(t *testing.T) {
	sb := pendingRunningSandbox()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := sb.waitForRunning(ctx, 2*time.Second)
	var timeoutErr *SandboxRunningTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want running timeout", err)
	}
	if time.Since(started) >= 500*time.Millisecond {
		t.Fatal("ignored earlier parent deadline")
	}
}

func TestWaitForRunningKeepsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pendingRunningSandbox().waitForRunning(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want parent cancellation", err)
	}
}
