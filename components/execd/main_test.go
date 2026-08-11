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
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

type fakeIsolatedRunnerCloser struct {
	closeFn func() error
}

func (f *fakeIsolatedRunnerCloser) Close() error {
	return f.closeFn()
}

func TestCloseIsolatedRunnerRetriesRetainedNamespaceOwnership(t *testing.T) {
	busyErr := errors.New("namespace pin is busy")
	closeCalls := 0
	var reported []error
	runner := &fakeIsolatedRunnerCloser{
		closeFn: func() error {
			closeCalls++
			if closeCalls == 1 {
				return errors.Join(
					runtime.ErrSessionNamespaceCleanup,
					busyErr,
				)
			}
			return nil
		},
	}

	if err := closeIsolatedRunnerWithRetry(
		runner,
		time.Second,
		time.Millisecond,
		func(err error) {
			reported = append(reported, err)
		},
	); err != nil {
		t.Fatal(err)
	}
	if closeCalls != 2 {
		t.Fatalf("Close calls = %d, want 2", closeCalls)
	}
	if len(reported) != 1 ||
		!errors.Is(reported[0], runtime.ErrSessionNamespaceCleanup) ||
		!errors.Is(reported[0], busyErr) {
		t.Fatalf("reported retry errors = %v", reported)
	}
}

func TestCloseIsolatedRunnerStopsAtRetryDeadline(t *testing.T) {
	busyErr := errors.New("namespace pin is permanently busy")
	closeCalls := 0
	runner := &fakeIsolatedRunnerCloser{
		closeFn: func() error {
			closeCalls++
			return errors.Join(
				runtime.ErrSessionNamespaceCleanup,
				busyErr,
			)
		},
	}

	err := closeIsolatedRunnerWithRetry(
		runner,
		10*time.Millisecond,
		time.Hour,
		nil,
	)
	if !errors.Is(err, runtime.ErrSessionNamespaceCleanup) ||
		!errors.Is(err, busyErr) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", closeCalls)
	}
}

func TestCloseIsolatedRunnerDoesNotRetryTerminalError(t *testing.T) {
	closeErr := errors.New("terminal cleanup error")
	closeCalls := 0
	runner := &fakeIsolatedRunnerCloser{
		closeFn: func() error {
			closeCalls++
			return closeErr
		},
	}

	err := closeIsolatedRunnerWithRetry(
		runner,
		time.Second,
		time.Millisecond,
		nil,
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v, want %v", err, closeErr)
	}
	if closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", closeCalls)
	}
}

func TestCloseIsolatedRunnerDoesNotRetryTeardownTimeout(t *testing.T) {
	closeCalls := 0
	runner := &fakeIsolatedRunnerCloser{
		closeFn: func() error {
			closeCalls++
			return runtime.ErrSessionTeardownTimeout
		},
	}

	err := closeIsolatedRunnerWithRetry(
		runner,
		time.Second,
		time.Millisecond,
		nil,
	)
	if !errors.Is(err, runtime.ErrSessionTeardownTimeout) {
		t.Fatalf(
			"close error = %v, want %v",
			err,
			runtime.ErrSessionTeardownTimeout,
		)
	}
	if closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", closeCalls)
	}
}

func TestServeHTTPUntilShutdownReturnsAfterContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveHTTPUntilShutdown(
			ctx,
			listener,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
		)
	}()

	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server did not stop after shutdown cancellation")
	}
}
