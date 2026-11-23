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

package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestInitHTTPProtocolSelection(t *testing.T) {
	for _, protocol := range []string{"http/protobuf", "http/json", "unknown"} {
		t.Run(protocol, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			endpoint, exported := newTestCollector(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv(envOTLPProtocol, "grpc")
			t.Setenv(envOTLPMetricsProtocol, protocol)
			t.Setenv(envOTLPEndpoint, "http://127.0.0.1:1")
			t.Setenv(envOTLPMetricsEndpoint, endpoint+"/v1/metrics")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{ServiceName: "http-selection-test"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := shutdown(ctx); err != nil {
					t.Error(err)
				}
			}()
			counter, err := otel.Meter("test").Int64Counter("http.counter")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(ctx, 1)
			if err := ForceFlush(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exported:
			case <-ctx.Done():
				t.Fatal("missing HTTP export for selected protocol")
			}
		})
	}
}
