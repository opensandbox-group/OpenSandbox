// Copyright 2025 The OpenSandbox Authors
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

package execute

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	execdflag "github.com/alibaba/opensandbox/execd/pkg/flag"
)

func createTestServer(t *testing.T, handleFunc func(conn *websocket.Conn)) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/kernels/") {
			t.Errorf("expected path to start with '/api/kernels/', got '%s'", r.URL.Path)
		}
		if !strings.HasSuffix(r.URL.Path, "/channels") {
			t.Errorf("expected path to end with '/channels', got '%s'", r.URL.Path)
		}

		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("failed to upgrade to WebSocket: %v", err)
		}
		defer conn.Close()

		handleFunc(conn)
	}))

	return server
}

func TestExecuteCodeStream(t *testing.T) {
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		for i := 0; i < 3; i++ {
			streamContent, _ := json.Marshal(StreamOutput{
				Name: StreamStdout,
				Text: "Line " + string(rune('0'+i)) + "\n",
			})

			streamMsg := Message{
				Header: Header{
					MessageID:   "stream-msg-id-" + string(rune('0'+i)),
					Session:     executeRequest.Header.Session,
					MessageType: string(MsgStream),
				},
				ParentHeader: executeRequest.Header,
				Content:      json.RawMessage(streamContent),
			}
			conn.WriteJSON(streamMsg)
			time.Sleep(100 * time.Millisecond)
		}

		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed",
			},
			Metadata: map[string]interface{}{},
		})

		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		conn.WriteJSON(executeResultMsg)

		statusContent, _ := json.Marshal(StatusUpdate{
			ExecutionState: StateIdle,
		})

		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		conn.WriteJSON(statusMsg)
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"

	client := NewClient("", nil)

	err := client.Connect(wsURL)
	if err != nil {
		t.Fatalf("failed to connect to WebSocket: %v", err)
	}
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	err = client.ExecuteCodeStream("for i in range(3):\n    print(f'Line {i}')", resultChan)
	if err != nil {
		t.Fatalf("failed to start streaming execution: %v", err)
	}

	resultCount := 0
	for result := range resultChan {
		if result == nil {
			break
		}
		resultCount++
	}

	if resultCount < 4 {
		t.Errorf("expected at least 4 results, got %d", resultCount)
	}
}

func TestExecuteCodeStreamWaitsForLateExecuteResultUsingConfiguredPollInterval(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	execdflag.JupyterIdlePollInterval = time.Millisecond
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
	})

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		require.NoError(t, conn.WriteJSON(statusMsg))

		time.Sleep(15 * time.Millisecond)

		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed late",
			},
			Metadata: map[string]interface{}{},
		})
		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		require.NoError(t, conn.WriteJSON(executeResultMsg))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	client := NewClient("", nil)
	require.NoError(t, client.Connect(wsURL))
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, client.ExecuteCodeStream("print('late result')", resultChan))

	start := time.Now()
	var gotLateResult bool
	for result := range resultChan {
		if result != nil && result.ExecutionCount == 1 {
			gotLateResult = true
		}
	}
	elapsed := time.Since(start)

	require.True(t, gotLateResult, "expected late execute_result to be delivered before stream close")
	require.Less(t, elapsed, 100*time.Millisecond, "expected stream to close promptly after late execute_result")
}

func TestExecuteCodeStreamFallsBackWhenPollIntervalIsNonPositive(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	execdflag.JupyterIdlePollInterval = 0
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
	})

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		require.NoError(t, conn.WriteJSON(statusMsg))

		time.Sleep(15 * time.Millisecond)

		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed with fallback",
			},
			Metadata: map[string]interface{}{},
		})
		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		require.NoError(t, conn.WriteJSON(executeResultMsg))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	client := NewClient("", nil)
	require.NoError(t, client.Connect(wsURL))
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, client.ExecuteCodeStream("print('fallback')", resultChan))

	start := time.Now()
	var gotLateResult bool
	for result := range resultChan {
		if result != nil && result.ExecutionCount == 1 {
			gotLateResult = true
		}
	}
	elapsed := time.Since(start)

	require.True(t, gotLateResult, "expected late execute_result to be delivered before stream close")
	require.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "expected non-positive poll interval to fall back to runtime default (100ms)")
	require.Less(t, elapsed, 300*time.Millisecond, "expected fallback poll interval to still close stream promptly")
}

// collectUntilClosed drains resultChan and fails the test if it is not closed
// within the timeout.
func collectUntilClosed(t *testing.T, resultChan chan *ExecutionResult, timeout time.Duration) []*ExecutionResult {
	t.Helper()
	var results []*ExecutionResult
	deadline := time.After(timeout)
	for {
		select {
		case result, ok := <-resultChan:
			if !ok {
				return results
			}
			results = append(results, result)
		case <-deadline:
			t.Fatalf("resultChan was not closed within %s; got %d results", timeout, len(results))
		}
	}
}

func sendStatus(conn *websocket.Conn, state ExecutionState, parent Header) {
	content, _ := json.Marshal(StatusUpdate{ExecutionState: state})
	_ = conn.WriteJSON(Message{
		Header:       Header{MessageID: "status-" + string(state), MessageType: string(MsgStatus)},
		ParentHeader: parent,
		Content:      json.RawMessage(content),
	})
}

func TestExecuteCodeStreamFailsWhenConnectionDrops(t *testing.T) {
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}
		streamContent, _ := json.Marshal(StreamOutput{Name: StreamStdout, Text: "before drop\n"})
		_ = conn.WriteJSON(Message{
			Header:       Header{MessageID: "stream-msg-id", MessageType: string(MsgStream)},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(streamContent),
		})
		// Returning closes the connection without an execute_reply.
	})
	defer server.Close()

	client := NewClient("", nil)
	if err := client.Connect("ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/k/channels"); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	if err := client.ExecuteCodeStream("print('before drop')", resultChan); err != nil {
		t.Fatalf("ExecuteCodeStream: %v", err)
	}

	results := collectUntilClosed(t, resultChan, 3*time.Second)
	last := results[len(results)-1]
	if last.Error == nil {
		t.Fatalf("last result = %+v, want an error", last)
	}
}

func TestExecuteCodeStreamFailsWhenKernelRestarts(t *testing.T) {
	serverDone := make(chan struct{})
	defer close(serverDone)
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}
		sendStatus(conn, StateBusy, executeRequest.Header)
		// The kernel died; the server restarts it, and the new kernel reports
		// idle with no parent. The execute_request never gets a reply.
		sendStatus(conn, StateRestarting, Header{})
		sendStatus(conn, StateStarting, Header{})
		sendStatus(conn, StateIdle, Header{})
		<-serverDone
	})
	defer server.Close()

	client := NewClient("", nil)
	if err := client.Connect("ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/k/channels"); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	if err := client.ExecuteCodeStream("import os; os._exit(1)", resultChan); err != nil {
		t.Fatalf("ExecuteCodeStream: %v", err)
	}

	results := collectUntilClosed(t, resultChan, 3*time.Second)
	if len(results) == 0 || results[len(results)-1].Error == nil {
		t.Fatalf("results = %+v, want the last one to be an error", results)
	}
	for _, result := range results {
		if result.ExecutionTime > 0 {
			t.Fatalf("got a completion result %+v for an execution that never finished", result)
		}
	}
}

func TestExecuteCodeStreamDisconnectIsNotReportedAsLostConnection(t *testing.T) {
	serverDone := make(chan struct{})
	defer close(serverDone)
	requestRead := make(chan struct{})
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}
		close(requestRead)
		<-serverDone
	})
	defer server.Close()

	client := NewClient("", nil)
	if err := client.Connect("ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/k/channels"); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	resultChan := make(chan *ExecutionResult, 10)
	if err := client.ExecuteCodeStream("import time; time.sleep(60)", resultChan); err != nil {
		t.Fatalf("ExecuteCodeStream: %v", err)
	}
	<-requestRead

	// The caller stopped waiting (e.g. its request was cancelled) and closes
	// the connection itself.
	client.Disconnect()
	time.Sleep(200 * time.Millisecond)
	select {
	case result := <-resultChan:
		t.Fatalf("got %+v after Disconnect, want nothing", result)
	default:
	}
}

func sendExecuteReply(conn *websocket.Conn, executionCount int, parent Header) {
	content, _ := json.Marshal(ExecuteReply{ExecutionCount: executionCount, Status: "ok"})
	_ = conn.WriteJSON(Message{
		Header:       Header{MessageID: "reply-msg-id", MessageType: string(MsgExecuteReply)},
		ParentHeader: parent,
		Content:      json.RawMessage(content),
	})
}

// A connection that closes after the kernel has replied does not turn the
// completed execution into a failure, whether or not idle arrived first.
func TestExecuteCodeStreamConnectionClosedAfterReplyIsNotAnError(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	execdflag.JupyterIdlePollInterval = 200 * time.Millisecond
	t.Cleanup(func() { execdflag.JupyterIdlePollInterval = previousPollInterval })

	for name, idleFirst := range map[string]bool{"idle then reply": true, "reply without idle": false} {
		t.Run(name, func(t *testing.T) {
			server := createTestServer(t, func(conn *websocket.Conn) {
				var executeRequest Message
				if err := conn.ReadJSON(&executeRequest); err != nil {
					return
				}
				if idleFirst {
					sendStatus(conn, StateIdle, executeRequest.Header)
					// Let finalizeExecution start waiting for the reply.
					time.Sleep(50 * time.Millisecond)
				}
				sendExecuteReply(conn, 1, executeRequest.Header)
				// Returning closes the connection right after the reply.
			})
			defer server.Close()

			client := NewClient("", nil)
			if err := client.Connect("ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/k/channels"); err != nil {
				t.Fatalf("failed to connect: %v", err)
			}
			defer client.Disconnect()

			resultChan := make(chan *ExecutionResult, 10)
			if err := client.ExecuteCodeStream("1", resultChan); err != nil {
				t.Fatalf("ExecuteCodeStream: %v", err)
			}

			results := collectUntilClosed(t, resultChan, 3*time.Second)
			completed := false
			for _, result := range results {
				if result.Error != nil {
					t.Fatalf("got error result %+v for an execution the kernel completed", result.Error)
				}
				if result.ExecutionTime > 0 {
					completed = true
				}
			}
			if !completed {
				t.Fatalf("results = %+v, want a completion", results)
			}
		})
	}
}
