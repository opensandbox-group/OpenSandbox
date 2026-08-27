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

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
)

func TestRuntimeFailureWaitsForSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		waitForRuntimeFailureSignal(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("runtime failure wait returned before shutdown signal")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime failure wait did not return after shutdown signal")
	}
}

func TestServerAddressesForInvalidConfig(t *testing.T) {
	tests := []struct {
		name       string
		serverAddr string
		wantHealth string
	}{
		{name: "valid health address is preserved", serverAddr: "127.0.0.1:18080", wantHealth: "127.0.0.1:18080"},
		{name: "wildcard health address is preserved", serverAddr: ":18080", wantHealth: ":18080"},
		{name: "localhost health address is preserved", serverAddr: "localhost:18080", wantHealth: "localhost:18080"},
		{name: "IPv6 health address is preserved", serverAddr: "[::1]:18080", wantHealth: "[::1]:18080"},
		{name: "invalid health host preserves port", serverAddr: "bad host:18080", wantHealth: ":18080"},
		{name: "unresolved health host preserves port", serverAddr: "missing.example:18080", wantHealth: ":18080"},
		{name: "malformed health address uses fallback", serverAddr: "invalid", wantHealth: ":8080"},
		{name: "invalid health port uses fallback", serverAddr: ":not-a-port", wantHealth: ":8080"},
		{name: "ephemeral health port uses fallback", serverAddr: ":0", wantHealth: ":8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthAddr, pprofAddr := serverAddresses(config.Config{ServerAddr: tt.serverAddr, PprofAddr: "127.0.0.1:6060"}, errors.New("invalid config"))
			if healthAddr != tt.wantHealth {
				t.Fatalf("health address = %q, want %q", healthAddr, tt.wantHealth)
			}
			if pprofAddr != "" {
				t.Fatalf("pprof address = %q, want disabled", pprofAddr)
			}
		})
	}
}

func TestServerAddressesForValidConfig(t *testing.T) {
	cfg := config.Config{ServerAddr: ":8080", PprofAddr: "127.0.0.1:6060"}
	healthAddr, pprofAddr := serverAddresses(cfg, nil)
	if healthAddr != cfg.ServerAddr || pprofAddr != cfg.PprofAddr {
		t.Fatalf("server addresses = %q, %q; want %q, %q", healthAddr, pprofAddr, cfg.ServerAddr, cfg.PprofAddr)
	}
}

func TestRunPipelineNotifiesOwner(t *testing.T) {
	wantErr := errors.New("pipeline failed")
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "error", err: wantErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan error, 1)
			runPipeline(done, func() error { return tt.err })
			if got := <-done; !errors.Is(got, tt.err) {
				t.Fatalf("completion error = %v, want %v", got, tt.err)
			}
		})
	}
}

func TestRunPipelineNotifiesOwnerBeforePropagatingPanic(t *testing.T) {
	done := make(chan error, 1)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		runPipeline(done, func() error { panic("boom") })
	}()

	if recovered != "boom" {
		t.Fatalf("recovered panic = %v, want boom", recovered)
	}
	if got := <-done; !errors.Is(got, errPipelinePanicked) {
		t.Fatalf("completion error = %v, want %v", got, errPipelinePanicked)
	}
}
