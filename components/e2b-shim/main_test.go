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

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Obviously fake secret values. Real credentials never appear in tests.
func serveEnv(listen string) map[string]string {
	return map[string]string{
		"E2B_SHIM_LISTEN":              listen,
		"E2B_SHIM_NATIVE_API_URL":      "https://native.example.internal/v1",
		"E2B_SHIM_ADAPTER_ID":          "e2b-shim-test",
		"E2B_SHIM_HMAC_KEY_VERSION":    "v1",
		"E2B_SHIM_TIMEOUT_MAX_SECONDS": "3600",
		"E2B_SHIM_ADAPTER_API_KEY":     "fake-adapter-api-key-DO-NOT-USE",
		"E2B_SHIM_NATIVE_API_KEY":      "fake-native-api-key-DO-NOT-USE",
		"E2B_SHIM_HMAC_KEY":            "fake-hmac-key-DO-NOT-USE",
	}
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// syncBuffer collects logger output written by another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRunHelpAndUsage(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"adapter", "--help"}, {"adapter", "-h"}, {"router", "-h"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut, lookup(nil)); code != exitOK {
			t.Errorf("%v: exit code = %d, want 0", args, code)
		}
		if strings.Count(out.String(), "Usage: e2b-shim") != 1 || errOut.Len() != 0 {
			t.Errorf("%v: stdout = %q, stderr = %q, want usage once on stdout", args, out.String(), errOut.String())
		}
	}
}

func TestRunVersion(t *testing.T) {
	if code := run(context.Background(), []string{"version"}, &bytes.Buffer{}, &bytes.Buffer{}, lookup(nil)); code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"bogus"}, &out, &errOut, lookup(nil)); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunNoArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut, lookup(nil)); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "Usage: e2b-shim") {
		t.Errorf("stderr = %q, want usage", errOut.String())
	}
}

func TestRunFlagErrors(t *testing.T) {
	env := serveEnv("127.0.0.1:0")
	for _, args := range [][]string{
		{"adapter", "--bogus"},
		{"adapter", "extra-arg"},
		{"router", "--listen"},
		{"router", "--config"},
	} {
		var out, errOut syncBuffer
		code := run(context.Background(), args, &out, &errOut, lookup(env))
		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestRunInvalidConfigDoesNotListen(t *testing.T) {
	env := serveEnv("127.0.0.1:0")
	delete(env, "E2B_SHIM_HMAC_KEY")
	var out, errOut syncBuffer
	code := run(context.Background(), []string{"adapter"}, &out, &errOut, lookup(env))
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "invalid configuration") {
		t.Errorf("stderr = %q, want a configuration error", stderr)
	}
	if strings.Contains(stderr, "listening") {
		t.Error("server started listening despite invalid configuration")
	}
	for _, secret := range []string{"fake-adapter-api-key-DO-NOT-USE", "fake-native-api-key-DO-NOT-USE"} {
		if strings.Contains(stderr, secret) {
			t.Errorf("stderr leaks %q", secret)
		}
	}
}

func TestRunInvalidListenDoesNotListen(t *testing.T) {
	env := serveEnv("not-an-address")
	var out, errOut syncBuffer
	code := run(context.Background(), []string{"router"}, &out, &errOut, lookup(env))
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if strings.Contains(errOut.String(), "listening") {
		t.Error("server started listening despite an invalid listen address")
	}
}

func TestRunExplicitEmptyConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	lookupEnv := func(string) (string, bool) {
		t.Error("environment read despite invalid --config flag")
		return "", false
	}
	code := run(context.Background(), []string{"adapter", "--config", ""}, &out, &errOut, lookupEnv)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "--config must not be empty") {
		t.Errorf("stderr = %q, want an empty flag error", errOut.String())
	}
}

func TestRunListenErrorDoesNotEchoAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"router"}, &out, &errOut, lookup(serveEnv(addr)))
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "listen failed: bind:") || strings.Contains(stderr, addr) {
		t.Errorf("stderr = %q, want the bind cause without the address", stderr)
	}
}

func TestRunServeStartsStopsAndServesHealthz(t *testing.T) {
	for _, role := range []string{"adapter", "router"} {
		t.Run(role, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out, errOut syncBuffer
			codeCh := make(chan int, 1)
			go func() {
				codeCh <- run(ctx, []string{role}, &out, &errOut, lookup(serveEnv("127.0.0.1:0")))
			}()

			addr := waitForListening(t, &errOut)
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get("http://" + addr + "/healthz")
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET /healthz: status = %d, want 200", resp.StatusCode)
			}
			resp.Body.Close()

			notFound, err := client.Get("http://" + addr + "/v2/sandboxes")
			if err != nil {
				t.Fatalf("GET /v2/sandboxes: %v", err)
			}
			notFound.Body.Close()
			if notFound.StatusCode != http.StatusNotFound {
				t.Errorf("GET /v2/sandboxes: status = %d, want 404", notFound.StatusCode)
			}

			cancel()
			select {
			case code := <-codeCh:
				if code != exitOK {
					t.Errorf("exit code = %d, want 0 after clean shutdown", code)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("process did not stop within 15s of cancellation")
			}
			if strings.Contains(errOut.String(), "fake-") {
				t.Error("startup output leaks secret material")
			}
		})
	}
}

// waitForListening extracts the bound address from the startup log line.
func waitForListening(t *testing.T, errOut *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(errOut.String(), "\n") {
			if strings.Contains(line, "msg=listening") {
				for _, field := range strings.Fields(line) {
					if addr, ok := strings.CutPrefix(field, "addr="); ok {
						return addr
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no listening line within 10s; stderr = %q", errOut.String())
	return ""
}
