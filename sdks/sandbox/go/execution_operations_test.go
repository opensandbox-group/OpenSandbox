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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionOperationsMapping(t *testing.T) {
	key := "instance.timestamp.caller"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "token", r.Header.Get("X-EXECD-ACCESS-TOKEN"))
		switch r.URL.Path {
		case "/execution/instance":
			fmt.Fprint(w, `{"instance_id":"scope","issued_at":123,"retention_seconds":86400,"capacity":4096}`)
			return
		case "/execution/operation":
			require.Equal(t, key, r.Header.Get("X-EXECD-OPERATION-ID"))
			require.Equal(t, "command", r.URL.Query().Get("kind"))
		case "/command/operations", "/pty/operations":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, key, body["operation_id"])
			require.Equal(t, "echo hello", body["command"])
			require.Equal(t, "/tmp", body["cwd"])
			w.WriteHeader(202)
		}
		fmt.Fprint(w, `{"id":"original","kind":"command","state":"creating","expires_at":"2026-09-09T00:00:00Z"}`)
	}))
	defer server.Close()
	client := NewExecdClient(server.URL, "token")
	ctx := context.Background()
	instance, err := client.GetExecutionInstance(ctx)
	require.NoError(t, err)
	identity, err := instance.NewOperationID()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(identity, "scope.123."))
	op, err := client.CreateCommandOperation(ctx, key, RunCommandRequest{Command: "echo hello", Cwd: "/tmp"})
	require.NoError(t, err)
	require.Equal(t, "creating", op.State)
	op, err = client.GetExecutionOperation(ctx, "command", key)
	require.NoError(t, err)
	require.Equal(t, "original", op.ID)
	_, err = client.CreatePTYOperation(ctx, key, "/tmp", "echo hello")
	require.NoError(t, err)
	require.Equal(t, 4, calls)
	_, err = client.CreateCommandOperation(ctx, "", RunCommandRequest{})
	require.Error(t, err)
	require.Equal(t, 4, calls)
}

func TestExecutionInstanceCache(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		fmt.Fprintf(w, `{"instance_id":"scope","issued_at":%d,"retention_seconds":86400,"capacity":4096}`, n)
	}))
	defer server.Close()
	client := NewExecdClient(server.URL, "token")
	ctx := context.Background()
	results := make(chan *ExecutionInstance, 16)
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := client.GetExecutionInstance(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		}()
	}
	<-entered
	cancelCtx, cancel := context.WithCancel(ctx)
	cancelled := make(chan error, 1)
	go func() { _, err := client.GetExecutionInstance(cancelCtx); cancelled <- err }()
	cancel()
	require.ErrorIs(t, <-cancelled, context.Canceled)
	close(release)
	workers.Wait()
	close(results)
	for result := range results {
		require.Equal(t, int64(1), result.IssuedAt)
		result.InstanceID = "caller-mutation"
	}
	result, err := client.GetExecutionInstance(ctx)
	require.NoError(t, err)
	require.Equal(t, "scope", result.InstanceID)
	require.Equal(t, int32(1), calls.Load())
	client.instanceCache.Lock()
	client.instanceCache.expires = time.Now().Add(-time.Second)
	client.instanceCache.Unlock()
	result, err = client.GetExecutionInstance(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), result.IssuedAt)
}

func TestExecutionInstanceInvalidationDoesNotReplay(t *testing.T) {
	for _, code := range []string{"operation_instance_mismatch", "operation_expired"} {
		for _, method := range []string{"command", "pty", "lookup"} {
			t.Run(code+"/"+method, func(t *testing.T) {
				var gets, operations atomic.Int32
				entered, release := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/execution/instance" {
						n := gets.Add(1)
						if n == 1 {
							close(entered)
							<-release
						}
						fmt.Fprintf(w, `{"instance_id":"scope-%d","issued_at":123,"retention_seconds":86400,"capacity":4096}`, n)
						return
					}
					operations.Add(1)
					if r.Method == "POST" {
						var body map[string]any
						json.NewDecoder(r.Body).Decode(&body)
						if body["operation_id"] != "saved.identity" {
							t.Error("identity changed")
						}
					} else if r.Header.Get("X-EXECD-OPERATION-ID") != "saved.identity" {
						t.Error("identity changed")
					}
					w.WriteHeader(409)
					fmt.Fprintf(w, `{"code":%q,"message":"unknown outcome"}`, code)
				}))
				defer server.Close()
				client := NewExecdClient(server.URL, "token")
				ctx := context.Background()
				old := make(chan error, 1)
				go func() { _, err := client.GetExecutionInstance(ctx); old <- err }()
				<-entered
				var err error
				switch method {
				case "command":
					_, err = client.CreateCommandOperation(ctx, "saved.identity", RunCommandRequest{Command: "true"})
				case "pty":
					_, err = client.CreatePTYOperation(ctx, "saved.identity", "", "")
				case "lookup":
					_, err = client.GetExecutionOperation(ctx, "command", "saved.identity")
				}
				require.Error(t, err)
				fresh, err := client.GetExecutionInstance(ctx)
				require.NoError(t, err)
				require.Equal(t, "scope-2", fresh.InstanceID)
				close(release)
				require.NoError(t, <-old)
				cached, err := client.GetExecutionInstance(ctx)
				require.NoError(t, err)
				require.Equal(t, "scope-2", cached.InstanceID)
				require.Equal(t, int32(2), gets.Load())
				require.Equal(t, int32(1), operations.Load())
			})
		}
	}
}

func TestExecutionInstanceFailureIsNotCached(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"code":"unavailable","message":"retry later"}`)
			return
		}
		fmt.Fprint(w, `{"instance_id":"scope","issued_at":123,"retention_seconds":86400,"capacity":4096}`)
	}))
	defer server.Close()
	client := NewExecdClient(server.URL, "token")
	_, err := client.GetExecutionInstance(context.Background())
	require.Error(t, err)
	instance, err := client.GetExecutionInstance(context.Background())
	require.NoError(t, err)
	require.Equal(t, "scope", instance.InstanceID)
	require.Equal(t, 2, calls)
}
