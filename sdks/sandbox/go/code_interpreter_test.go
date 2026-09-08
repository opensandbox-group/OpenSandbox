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
	"sync/atomic"
	"testing"
	"time"
)

// newCodeInterpreterTestServer builds an execd test server that answers /ping
// and streams /command results. runtimeOK controls whether the runtime process
// check command succeeds; commandCalls counts /command requests.
func newCodeInterpreterTestServer(t *testing.T, runtimeOK func(attempt int32) bool, commandCalls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			w.WriteHeader(http.StatusOK)
		case "/command":
			var req RunCommandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			attempt := atomic.AddInt32(commandCalls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			if runtimeOK(attempt) {
				fmt.Fprint(w, "data: {\"type\":\"execution_complete\",\"execution_time\":1}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"type\":\"error\",\"ename\":\"CommandExecError\",\"evalue\":\"1\"}\n\n")
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestCodeInterpreter(t *testing.T, url string) *CodeInterpreter {
	t.Helper()
	sb := &Sandbox{
		id:    "ci-test-sandbox",
		execd: NewExecdClient(url, "tok"),
	}
	return &CodeInterpreter{Sandbox: sb}
}

func TestCodeInterpreter_IsHealthy(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return true }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	require.True(t, ci.IsHealthy(context.Background()), "IsHealthy should pass when the runtime process is up")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "runtime check should run once")
}

func TestCodeInterpreter_IsHealthy_FailsWhenRuntimeProcessMissing(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return false }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	require.True(t, !ci.IsHealthy(context.Background()), "IsHealthy should fail when the runtime process is missing")
	require.True(t, atomic.LoadInt32(&calls) >= 1, "runtime check should have been attempted")
}

func TestCodeInterpreter_WaitRuntimeReady_PollsUntilRuntimeUp(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return attempt >= 3 }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	err := ci.waitRuntimeReady(context.Background(), 5*time.Second, 5*time.Millisecond)
	require.NoError(t, err, "waitRuntimeReady")
	require.Equal(t, int32(3), atomic.LoadInt32(&calls), "runtime check should pass on the third attempt")
}

func TestCodeInterpreter_WaitRuntimeReady_TimesOut(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return false }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	err := ci.waitRuntimeReady(context.Background(), 50*time.Millisecond, 5*time.Millisecond)
	require.Error(t, err, "waitRuntimeReady should time out")
	var readyErr *SandboxReadyTimeoutError
	require.ErrorAs(t, err, &readyErr, "timeout error type")
	assert.Contains(t, readyErr.Error(), "jupyter", "timeout message should mention the runtime")
}
