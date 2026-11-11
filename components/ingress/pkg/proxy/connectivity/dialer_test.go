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

package connectivity

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestWrapDialContextObservesTCPAttempt(t *testing.T) {
	var got Observation
	times := []time.Time{
		time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 26, 10, 0, 0, 25_000_000, time.UTC),
	}
	now := func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	next := func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ETIMEDOUT }
	observer := ObserverFunc(func(observation Observation) { got = observation })

	_, err := wrapDialContext(next, observer, "http", now)(context.Background(), "tcp", "Sandbox.EXAMPLE.:28888")
	if !errors.Is(err, syscall.ETIMEDOUT) {
		t.Fatalf("wrapped dial error = %v, want timeout", err)
	}
	if got.Result != ResultTimeout || got.Protocol != "http" || got.Target != "Sandbox.EXAMPLE.:28888" || got.Duration != 25*time.Millisecond {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestWrapDialContextPreservesNetworkErrorWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got Observation
	observer := ObserverFunc(func(observation Observation) { got = observation })
	next := func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ENETUNREACH }

	_, _ = WrapDialContext(next, observer, "websocket")(ctx, "tcp", "10.0.0.1:80")
	if got.Result != ResultUnreachable {
		t.Fatalf("result = %q, want %q", got.Result, ResultUnreachable)
	}
}

func TestWrapDialContextUsesCancellationAsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got Observation
	observer := ObserverFunc(func(observation Observation) { got = observation })
	next := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial interrupted") }

	_, _ = WrapDialContext(next, observer, "http")(ctx, "tcp", "10.0.0.1:80")
	if got.Result != ResultCanceled {
		t.Fatalf("result = %q, want %q", got.Result, ResultCanceled)
	}
}

func TestWrapDialContextAllowsNilBaseDialer(t *testing.T) {
	if wrapped := WrapDialContext(nil, ObserverFunc(func(Observation) {}), "http"); wrapped != nil {
		t.Fatalf("WrapDialContext(nil, ...) = %v, want nil", wrapped)
	}
}
