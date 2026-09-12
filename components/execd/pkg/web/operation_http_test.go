// Copyright 2025 Alibaba Group Holding Ltd.
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

//go:build !windows

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

// The application loses the body AFTER execd has handled the real request.
// The test retains a private copy solely to compare the server's handles.
func postOperation(t *testing.T, base, path string, body any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	result, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, result
}

func TestBaselineLostCreateResponse(t *testing.T) {
	if os.Getenv("EXECD_BASELINE_REPRO") != "1" {
		t.Skip("explicit baseline reproduction only")
	}
	controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "starts")
			body := map[string]any{"command": "echo $$ >> '" + marker + "'; sleep 0.1", "background": background, "operation_id": "same-logical-operation"}
			status, lost := postOperation(t, server.URL, "/command", body)
			require.Equal(t, 200, status)
			status, retry := postOperation(t, server.URL, "/command", body)
			require.Equal(t, 200, status)
			require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 2 }, 5*time.Second, 10*time.Millisecond)
			b, err := os.ReadFile(marker)
			require.NoError(t, err)
			t.Logf("lost response=%s retry response=%s process PIDs=%s", lost, retry, b)
			require.NotEqual(t, string(lost), string(retry))
		})
	}
}

func getOperationHTTP(t *testing.T, base, path, identity, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	require.NoError(t, err)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", token)
	req.Header.Set("X-EXECD-OPERATION-ID", identity)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

func operationIdentity(t *testing.T, base, suffix string) string {
	t.Helper()
	status, body := getOperationHTTP(t, base, "/execution/instance", "", "test-token")
	require.Equal(t, 200, status)
	var instance runtime.OperationInstance
	require.NoError(t, json.Unmarshal(body, &instance))
	return fmt.Sprintf("%s.%d.%s", instance.InstanceID, instance.IssuedAt, suffix)
}

func decodeOperation(t *testing.T, status int, body []byte) runtime.Operation {
	t.Helper()
	require.Contains(t, []int{200, 202}, status, string(body))
	var op runtime.Operation
	require.NoError(t, json.Unmarshal(body, &op))
	require.NotEmpty(t, op.ID)
	return op
}

func TestOperationHTTP(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-response-background-%v", background), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "starts")
			gate := filepath.Join(t.TempDir(), "release")
			key := operationIdentity(t, server.URL, fmt.Sprintf("response-%v", background))
			body := map[string]any{"operation_id": key, "command": "echo $$ >> '" + marker + "'; while [ ! -f '" + gate + "' ]; do sleep 0.01; done", "background": background}
			// Discard the application response; only test instrumentation retains its ID.
			status, lost := loseOperationResponse(t, server.URL, "/command/operations", body, func() {
				require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
			})
			original := decodeOperation(t, status, lost)
			defer os.WriteFile(gate, []byte("release"), 0600)
			require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
			status, data := postOperation(t, server.URL, "/command/operations", body)
			recovered := decodeOperation(t, status, data)
			require.Equal(t, original.ID, recovered.ID)
			require.Equal(t, "created", recovered.State)
			status, data = getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "test-token")
			require.Equal(t, original.ID, decodeOperation(t, status, data).ID)
			kernel, err := ctrl.GetCommandStatus(original.ID)
			require.NoError(t, err)
			require.True(t, kernel.Running)
			require.Equal(t, body["command"], kernel.Content)
			b, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Len(t, strings.Fields(string(b)), 1)
			require.NoError(t, os.WriteFile(gate, []byte("release"), 0600))
			require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(original.ID); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
			status, data = postOperation(t, server.URL, "/command/operations", body)
			require.Equal(t, original.ID, decodeOperation(t, status, data).ID)
			b, err = os.ReadFile(marker)
			require.NoError(t, err)
			require.Len(t, strings.Fields(string(b)), 1)
			t.Logf("original=%s recovered=%s one process PID=%s", original.ID, recovered.ID, strings.TrimSpace(string(b)))
		})
	}
	t.Run("concurrent-same-and-different-identities", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "starts")
		key := operationIdentity(t, server.URL, "concurrency")
		gate := make(chan struct{})
		type result struct {
			status int
			data   []byte
			err    error
		}
		results := make(chan result, 16)
		for i := 0; i < 16; i++ {
			go func() {
				<-gate
				b, _ := json.Marshal(map[string]any{"operation_id": key, "command": "echo $$ >> '" + marker + "'"})
				req, _ := http.NewRequest("POST", server.URL+"/command/operations", bytes.NewReader(b))
				req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					results <- result{err: err}
					return
				}
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				results <- result{resp.StatusCode, data, err}
			}()
		}
		close(gate)
		var id string
		for i := 0; i < 16; i++ {
			res := <-results
			require.NoError(t, res.err)
			op := decodeOperation(t, res.status, res.data)
			if id == "" {
				id = op.ID
			}
			require.Equal(t, id, op.ID)
		}
		require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(id); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
		b, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Len(t, strings.Fields(string(b)), 1)
		other := operationIdentity(t, server.URL, "different-key")
		status, data := postOperation(t, server.URL, "/command/operations", map[string]any{"operation_id": other, "command": "echo $$ >> '" + marker + "'"})
		second := decodeOperation(t, status, data)
		require.NotEqual(t, id, second.ID)
		require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(second.ID); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
		b, err = os.ReadFile(marker)
		require.NoError(t, err)
		require.Len(t, strings.Fields(string(b)), 2)
	})
	t.Run("conflict-auth-secrets-restart", func(t *testing.T) {
		key := operationIdentity(t, server.URL, "private-key")
		body := map[string]any{"operation_id": key, "command": "true", "envs": map[string]string{"SECRET": "do-not-disclose"}}
		status, data := postOperation(t, server.URL, "/command/operations", body)
		original := decodeOperation(t, status, data)
		body["command"] = "echo do-not-disclose"
		status, data = postOperation(t, server.URL, "/command/operations", body)
		require.Equal(t, 409, status)
		require.NotContains(t, string(data), "do-not-disclose")
		require.NotContains(t, string(data), key)
		status, data = getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "wrong-token")
		require.Equal(t, 401, status)
		require.NotContains(t, string(data), original.ID)
		status, _ = getOperationHTTP(t, server.URL, "/execution/operation?kind=pty", key, "test-token")
		require.Equal(t, 404, status)
		require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(original.ID); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
		controller.InitCodeRunner()
		status, data = postOperation(t, server.URL, "/command/operations", body)
		require.Equal(t, 409, status)
		require.Contains(t, string(data), "operation_instance_mismatch")
	})
}

// A real HTTP fault proxy consumes execd's response, waits at a controlled point,
// then closes the downstream socket without delivering status or body to the caller.
func loseOperationResponse(t *testing.T, base, path string, body any, afterAccepted func()) (int, []byte) {
	t.Helper()
	type captured struct {
		status int
		data   []byte
		err    error
	}
	accepted := make(chan captured, 1)
	release := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest("POST", base+path, r.Body)
		if err != nil {
			accepted <- captured{err: err}
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			accepted <- captured{err: err}
			return
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		accepted <- captured{resp.StatusCode, data, err}
		<-release
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer proxy.Close()
	defer close(release)
	b, err := json.Marshal(body)
	require.NoError(t, err)
	clientDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("POST", proxy.URL, bytes.NewReader(b))
		req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	capturedResponse := <-accepted
	require.NoError(t, capturedResponse.err)
	if afterAccepted != nil {
		afterAccepted()
	}
	// Release in the deferred function after saving the instrumentation result.
	t.Cleanup(func() { require.Error(t, <-clientDone, "caller must lose the response") })
	return capturedResponse.status, capturedResponse.data
}

func TestOperationPTYHTTP(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "starts")
	gate := filepath.Join(t.TempDir(), "release")
	key := operationIdentity(t, server.URL, "pty-recovery")
	body := map[string]any{"operation_id": key, "command": "echo $$ >> '" + marker + "'; echo replay-marker; while [ ! -f '" + gate + "' ]; do sleep 0.01; done; exit 7"}
	status, lost := loseOperationResponse(t, server.URL, "/pty/operations", body, nil)
	original := decodeOperation(t, status, lost)
	defer ctrl.DeletePTYSession(original.ID)
	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "POST must not start the PTY process")
	status, data := postOperation(t, server.URL, "/pty/operations", body)
	require.Equal(t, original.ID, decodeOperation(t, status, data).ID)
	require.Eventually(t, func() bool {
		status, data := getOperationHTTP(t, server.URL, "/execution/operation?kind=pty", key, "test-token")
		return decodeOperation(t, status, data).State == "created"
	}, 5*time.Second, time.Millisecond)
	var state model.PTYSessionStatusResponse
	getExecutionStatusHTTP(t, server.URL, "/pty/"+original.ID, &state)
	require.False(t, state.LaunchAttempted)
	require.False(t, state.LaunchFailed)
	dial := func(query string) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/pty/"+original.ID+"/ws"+query, http.Header{"X-EXECD-ACCESS-TOKEN": []string{"test-token"}})
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	first := dial("")
	require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
	// Drain until output reaches the first connection, so replay has a known source.
	readUntil := func(conn *websocket.Conn, needle string) {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, frame, err := conn.ReadMessage()
			require.NoError(t, err)
			if strings.Contains(string(frame), needle) {
				return
			}
		}
	}
	readUntil(first, "replay-marker")
	getExecutionStatusHTTP(t, server.URL, "/pty/"+original.ID, &state)
	require.True(t, state.Running)
	require.True(t, state.LaunchAttempted)
	require.False(t, state.LaunchFailed)
	second := dial("?takeover=1&since=0")
	readUntil(second, "replay-marker")
	b, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(b)), 1)
	require.NoError(t, os.WriteFile(gate, []byte("release"), 0600))
	require.Eventually(t, func() bool { running, _, err := ctrl.GetPTYSessionStatus(original.ID); return err == nil && !running }, 5*time.Second, 10*time.Millisecond)
	second.Close()
	third := dial("?takeover=1&since=0")
	readUntil(third, "replay-marker")
	getExecutionStatusHTTP(t, server.URL, "/pty/"+original.ID, &state)
	require.False(t, state.Running)
	require.True(t, state.LaunchAttempted)
	require.False(t, state.LaunchFailed, "nonzero exit and rejected relaunch are not startup failures")
	require.Equal(t, 7, ctrl.GetPTYSession(original.ID).ExitCode())
	status, data = postOperation(t, server.URL, "/pty/operations", body)
	require.Equal(t, original.ID, decodeOperation(t, status, data).ID)
	b, err = os.ReadFile(marker)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(b)), 1)
	t.Logf("lost PTY handle=%s; recovered, attached, replayed, took over, reconnected after exit; one process PID=%s", original.ID, strings.TrimSpace(string(b)))
}

func TestOperationHTTPFailuresAndExpiry(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	key := operationIdentity(t, server.URL, "failed-launch")
	body := map[string]any{"operation_id": key, "command": "true", "cwd": "/does-not-exist-operation-test"}
	status, data := postOperation(t, server.URL, "/command/operations", body)
	first := decodeOperation(t, status, data)
	require.Eventually(t, func() bool {
		status, data = getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "test-token")
		return decodeOperation(t, status, data).State == "failed"
	}, 5*time.Second, 10*time.Millisecond)
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, first.ID, decodeOperation(t, status, data).ID)
	require.NotContains(t, string(data), body["cwd"])
	// A terminal execution failure is still successful creation, not permission to retry.
	key = operationIdentity(t, server.URL, "nonzero-exit")
	body = map[string]any{"operation_id": key, "command": "exit 7"}
	status, data = postOperation(t, server.URL, "/command/operations", body)
	first = decodeOperation(t, status, data)
	require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(first.ID); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, "created", decodeOperation(t, status, data).State)
	st, err := ctrl.GetCommandStatus(first.ID)
	require.NoError(t, err)
	require.Equal(t, 7, *st.ExitCode)
	parts := strings.Split(key, ".")
	parts[1] = "1"
	body["operation_id"] = strings.Join(parts, ".")
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, 410, status)
	require.Contains(t, string(data), "operation_expired")
	body["operation_id"] = operationIdentity(t, server.URL, "unknown-fields")
	body["future_secret_option"] = "must-not-leak"
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, 400, status)
	require.NotContains(t, string(data), "future_secret_option")
	require.NotContains(t, string(data), "must-not-leak")
	status, data = postOperation(t, server.URL, "/command/operations", map[string]any{
		"operation_id": operationIdentity(t, server.URL, "timeout-overflow"), "command": "true", "timeout": int64(9223372036855),
	})
	require.Equal(t, 400, status, string(data))
	// Old clients keep their response formats and repeated creation behavior.
	status, data = postOperation(t, server.URL, "/command", map[string]any{"command": "true"})
	require.Equal(t, 200, status)
	require.Contains(t, string(data), "execution_complete")
	status, data = postOperation(t, server.URL, "/pty", map[string]any{})
	require.Equal(t, 201, status)
	var legacy struct {
		ID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(data, &legacy))
	require.NotEmpty(t, legacy.ID)
	require.NoError(t, ctrl.DeletePTYSession(legacy.ID))
}

func TestOperationHTTPConcurrentDistinctAndRemovedCwd(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	gate := filepath.Join(t.TempDir(), "release")
	defer os.WriteFile(gate, []byte("release"), 0600)
	type attempt struct {
		body            map[string]any
		marker, cwd, id string
	}
	attempts := make([]attempt, 2)
	for i := range attempts {
		cwd := t.TempDir()
		marker := filepath.Join(t.TempDir(), "starts")
		attempts[i] = attempt{body: map[string]any{
			"operation_id": operationIdentity(t, server.URL, fmt.Sprintf("distinct-%d", i)),
			"cwd":          cwd, "command": "echo $$ >> '" + marker + "'; while [ ! -f '" + gate + "' ]; do sleep 0.01; done",
		}, marker: marker, cwd: cwd}
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range attempts {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			status, data := postOperation(t, server.URL, "/command/operations", attempts[i].body)
			attempts[i].id = decodeOperation(t, status, data).ID
		}(i)
	}
	close(start)
	group.Wait()
	require.NotEqual(t, attempts[0].id, attempts[1].id)
	for _, a := range attempts {
		require.Eventually(t, func() bool { b, _ := os.ReadFile(a.marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
		state, err := ctrl.GetCommandStatus(a.id)
		require.NoError(t, err)
		require.True(t, state.Running, "both distinct processes overlap at the release barrier")
	}
	require.NoError(t, os.WriteFile(gate, []byte("release"), 0600))
	for _, a := range attempts {
		require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(a.id); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, os.Remove(a.cwd))
		status, data := postOperation(t, server.URL, "/command/operations", a.body)
		require.Equal(t, a.id, decodeOperation(t, status, data).ID, "recovery must not revalidate a removed cwd")
		b, err := os.ReadFile(a.marker)
		require.NoError(t, err)
		require.Len(t, strings.Fields(string(b)), 1)
	}
}

// holdCreationResponse cancels the actual HTTP request after the runtime claim.
type holdCreationResponse struct {
	http.ResponseWriter
	ctx      context.Context
	accepted chan struct{}
}

func (w *holdCreationResponse) WriteHeader(status int) {
	close(w.accepted)
	<-w.ctx.Done()
	w.ResponseWriter.WriteHeader(status)
}

func TestOperationHTTPRequestCancellation(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	router := NewRouter("test-token")
	accepted := make(chan struct{})
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && first.CompareAndSwap(false, true) {
			router.ServeHTTP(&holdCreationResponse{w, r.Context(), accepted}, r)
		} else {
			router.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	key := operationIdentity(t, server.URL, "cancelled-request")
	marker := filepath.Join(t.TempDir(), "starts")
	gate := filepath.Join(t.TempDir(), "release")
	defer os.WriteFile(gate, []byte("release"), 0600)
	body := map[string]any{"operation_id": key, "command": "echo $$ >> '" + marker + "'; while [ ! -f '" + gate + "' ]; do sleep 0.01; done", "background": true}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/command/operations", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("creation was not accepted")
	}
	require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-clientDone, context.Canceled)
	status, data := getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "test-token")
	op := decodeOperation(t, status, data)
	st, err := ctrl.GetCommandStatus(op.ID)
	require.NoError(t, err)
	require.True(t, st.Running)
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, op.ID, decodeOperation(t, status, data).ID)
	require.NoError(t, os.WriteFile(gate, []byte("release"), 0600))
	require.Eventually(t, func() bool { s, _ := ctrl.GetCommandStatus(op.ID); return s != nil && !s.Running }, 5*time.Second, 10*time.Millisecond)
	b, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(b)), 1)
}

// Run with EXECD_SMOKE_BINARY pointing at a built execd executable. This exercises
// main/startup, the real listening server, auth, runtime and OS children together.
func TestOperationDaemonHTTP(t *testing.T) {
	binary := os.Getenv("EXECD_SMOKE_BINARY")
	if binary == "" {
		t.Skip("set EXECD_SMOKE_BINARY for the standalone-daemon smoke")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	launch := func() func() {
		file, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
		require.NoError(t, err)
		cmd := exec.Command(binary, fmt.Sprintf("--port=%d", port), "--access-token=test-token", "--operation-capacity=37")
		cmd.Env = append(os.Environ(), "EXECD_OPERATION_CAPACITY=41")
		cmd.Stdout = file
		cmd.Stderr = file
		require.NoError(t, cmd.Start())
		stop := sync.OnceFunc(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait(); _ = file.Close() })
		t.Cleanup(stop)
		require.Eventually(t, func() bool {
			req, _ := http.NewRequest("GET", base+"/execution/instance", nil)
			req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			var instance struct{ Capacity int }
			return resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&instance) == nil && instance.Capacity == 37
		}, 10*time.Second, 20*time.Millisecond)
		return stop
	}
	stop := launch()
	var saved map[string]any
	var finalMarker string
	for _, background := range []bool{false, true} {
		marker := filepath.Join(t.TempDir(), "starts")
		identity := operationIdentity(t, base, fmt.Sprintf("daemon-%v", background))
		body := map[string]any{"operation_id": identity, "command": "echo $$ >> '" + marker + "'; sleep 0.1", "background": background}
		status, data := loseOperationResponse(t, base, "/command/operations", body, func() {
			require.Eventually(t, func() bool { b, _ := os.ReadFile(marker); return len(strings.Fields(string(b))) == 1 }, 5*time.Second, 10*time.Millisecond)
		})
		original := decodeOperation(t, status, data)
		status, data = postOperation(t, base, "/command/operations", body)
		require.Equal(t, original.ID, decodeOperation(t, status, data).ID)
		require.Eventually(t, func() bool {
			req, _ := http.NewRequest("GET", base+"/command/status/"+original.ID, nil)
			req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			var state struct{ Running bool }
			return json.NewDecoder(resp.Body).Decode(&state) == nil && !state.Running
		}, 5*time.Second, 10*time.Millisecond)
		b, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Len(t, strings.Fields(string(b)), 1)
		t.Logf("standalone daemon background=%v handle=%s one process PID=%s", background, original.ID, strings.TrimSpace(string(b)))
		saved = body
		finalMarker = marker
	}
	stop()
	launch()
	status, data := postOperation(t, base, "/command/operations", saved)
	require.Equal(t, 409, status)
	require.Contains(t, string(data), "operation_instance_mismatch")
	b, err := os.ReadFile(finalMarker)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(b)), 1)
	t.Log("daemon restart rejected old identity; no second process")
}

func TestOperationAuthenticationErrorContract(t *testing.T) {
	router := NewRouter("test-token")
	for _, token := range []string{"", "wrong-token"} {
		for _, path := range []string{"/execution/instance", "/execution/operation?kind=command", "/command/operations", "/pty/operations", "/command", "/pty"} {
			req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			if strings.HasPrefix(path, "/execution/") {
				req.Method = "GET"
			}
			req.Header.Set("X-EXECD-ACCESS-TOKEN", token)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, 401, w.Code)
			var body map[string]string
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			if path == "/command" || path == "/pty" {
				require.Contains(t, body, "error")
				require.NotContains(t, body, "code")
			} else {
				require.Equal(t, "UNAUTHORIZED", body["code"])
				require.NotEmpty(t, body["message"])
				require.NotContains(t, body, "error")
			}
			require.NotContains(t, w.Body.String(), "test-token")
		}
	}
}
