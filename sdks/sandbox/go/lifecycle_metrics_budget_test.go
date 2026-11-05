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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type metricBudgetTransport func(*http.Request) (*http.Response, error)

func (f metricBudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetricRequestHasBoundedContextWithCustomClient(t *testing.T) {
	t.Setenv(disableMetricsEnv, "0")
	for _, timeout := range []time.Duration{0, time.Minute, 500 * time.Millisecond} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			observed := make(chan context.Context, 1)
			transport := metricBudgetTransport(func(r *http.Request) (*http.Response, error) {
				observed <- r.Context()
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			client := &http.Client{Transport: transport, Timeout: timeout}
			start := time.Now()
			reportSandboxCreateMetric(ConnectionConfig{Domain: "http://metrics.test", HTTPClient: client}, "sandbox", "image", 1, true)
			select {
			case ctx := <-observed:
				deadline, ok := ctx.Deadline()
				wantBudget := 5 * time.Second
				if timeout > 0 && timeout < wantBudget {
					wantBudget = timeout
				}
				if !ok || deadline.Before(start.Add(wantBudget)) || deadline.After(start.Add(wantBudget+time.Second)) {
					t.Errorf("metrics context deadline = %v, present=%v; want budget %v", deadline, ok, wantBudget)
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
					t.Error("metrics context was not released after request completed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("metric was not sent")
			}
			if client.Timeout != timeout {
				t.Errorf("caller client timeout changed: %v", client.Timeout)
			}
		})
	}
}
