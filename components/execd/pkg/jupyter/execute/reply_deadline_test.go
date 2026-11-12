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
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	execdflag "github.com/alibaba/opensandbox/execd/pkg/flag"
)

// An execution whose matching idle status never arrives but whose execute_reply
// did must not wait forever: once the kernel has replied, the cell is finished
// and the stream has to close within the reply-finalize deadline, delivering
// whatever output made it through. Without this bound, the SSE response stays
// open with keepalive pings, the session lock is never released, and the
// caller blocks indefinitely.
func TestExecuteCodeStreamFinalizesAfterReplyDeadlineWhenIdleNeverArrives(t *testing.T) {
	previous := execdflag.JupyterReplyFinalizeTimeout
	execdflag.JupyterReplyFinalizeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { execdflag.JupyterReplyFinalizeTimeout = previous })

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}

		streamContent, _ := json.Marshal(StreamOutput{Name: StreamStdout, Text: "resolved\n"})
		require.NoError(t, conn.WriteJSON(Message{
			Header:       Header{MessageID: "stream-msg-id", MessageType: string(MsgStream)},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(streamContent),
		}))

		sendExecuteReply(conn, 1, executeRequest.Header)

		// Hold the connection open but stay silent: the matching idle never
		// arrives. ReadMessage blocks until the test server closes the socket.
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	client := NewClient("", nil)
	require.NoError(t, client.Connect(wsURL))
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, client.ExecuteCodeStream("print('resolved')", resultChan))

	type drainOutcome struct {
		sawStdout   bool
		sawTerminal bool
	}
	start := time.Now()
	done := make(chan drainOutcome, 1)
	go func() {
		var out drainOutcome
		for result := range resultChan {
			if result == nil {
				break
			}
			if len(result.Stream) == 1 && result.Stream[0].Text == "resolved\n" {
				out.sawStdout = true
			}
			if result.ExecutionTime > 0 && result.Error == nil && len(result.Stream) == 0 {
				out.sawTerminal = true
			}
		}
		done <- out
	}()

	select {
	case out := <-done:
		require.Less(t, time.Since(start), 3*time.Second, "reply deadline must close the stream promptly")
		require.True(t, out.sawStdout, "expected the stdout result delivered before the deadline to be forwarded")
		require.True(t, out.sawTerminal, "expected a terminal complete result once the reply deadline fired")
	case <-time.After(5 * time.Second):
		t.Fatal("results channel never closed; reply deadline did not release the execution")
	}
}

// A matching idle that arrives only after the reply deadline already finalized
// the execution must be a harmless no-op: it must not re-finalize, send a
// second terminal result, or wedge the drain.
func TestExecuteCodeStreamLateIdleAfterReplyDeadlineIsHarmless(t *testing.T) {
	previous := execdflag.JupyterReplyFinalizeTimeout
	execdflag.JupyterReplyFinalizeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { execdflag.JupyterReplyFinalizeTimeout = previous })

	releaseIdle := make(chan struct{})
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}
		sendExecuteReply(conn, 1, executeRequest.Header)
		<-releaseIdle

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		require.NoError(t, conn.WriteJSON(Message{
			Header:       Header{MessageID: "status-msg-id", MessageType: string(MsgStatus)},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}))
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	client := NewClient("", nil)
	require.NoError(t, client.Connect(wsURL))
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, client.ExecuteCodeStream("print('resolved')", resultChan))

	terminals := 0
	for result := range resultChan {
		if result == nil {
			break
		}
		if result.ExecutionTime > 0 && result.Error == nil && len(result.Stream) == 0 {
			terminals++
		}
	}
	require.Equal(t, 1, terminals, "exactly one terminal complete result must be delivered")

	// Now let the late matching idle through and give the receive goroutine a
	// moment to dispatch it. The execution is already finalized: the channel
	// stays closed and no second terminal may appear.
	close(releaseIdle)
	time.Sleep(200 * time.Millisecond)

	select {
	case result := <-resultChan:
		if result != nil {
			t.Fatalf("late idle produced an extra result after finalization: %+v", result)
		}
	default:
	}
}

// The reply deadline and the idle-triggered finalization may race; exactly one
// of them must win the executeDone CAS and deliver exactly one terminal result.
func TestReplyDeadlineAndIdleFinalizationDeliverExactlyOneTerminal(t *testing.T) {
	previous := execdflag.JupyterReplyFinalizeTimeout
	execdflag.JupyterReplyFinalizeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { execdflag.JupyterReplyFinalizeTimeout = previous })

	client := &Client{handlers: make(map[MessageType]func(*Message))}
	state := newStreamExecutionState(time.Now())
	state.requestID = "req-1"
	parent := Header{MessageID: "req-1"}
	resultChan := make(chan *ExecutionResult, 10)

	// Dispatching the matching execute_reply arms the reply deadline; the idle
	// that follows races it. Whichever wins must produce exactly one terminal
	// result and close the channel.
	client.registerExecuteCodeStreamHandlers(state, resultChan)
	dispatchTestKernelMessage(t, client, MsgExecuteReply, ExecuteReply{ExecutionCount: 1, Status: "ok"}, parent)
	dispatchTestKernelMessage(t, client, MsgStatus, StatusUpdate{ExecutionState: StateIdle}, parent)

	terminals := 0
	for result := range resultChan {
		if result == nil {
			break
		}
		if result.ExecutionTime > 0 && result.Error == nil && len(result.Stream) == 0 {
			terminals++
		}
	}
	require.Equal(t, 1, terminals, "exactly one terminal complete result must be delivered")
}

// The normal path is unaffected: idle arrives right after the reply and the
// stream closes long before the reply deadline could fire.
func TestExecuteCodeStreamIdleBeforeReplyDeadlineFinalizesNormally(t *testing.T) {
	previous := execdflag.JupyterReplyFinalizeTimeout
	execdflag.JupyterReplyFinalizeTimeout = 5 * time.Second
	t.Cleanup(func() { execdflag.JupyterReplyFinalizeTimeout = previous })

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		if err := conn.ReadJSON(&executeRequest); err != nil {
			return
		}

		streamContent, _ := json.Marshal(StreamOutput{Name: StreamStdout, Text: "hi\n"})
		require.NoError(t, conn.WriteJSON(Message{
			Header:       Header{MessageID: "stream-msg-id", MessageType: string(MsgStream)},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(streamContent),
		}))

		sendExecuteReply(conn, 1, executeRequest.Header)

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		require.NoError(t, conn.WriteJSON(Message{
			Header:       Header{MessageID: "status-msg-id", MessageType: string(MsgStatus)},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	client := NewClient("", nil)
	require.NoError(t, client.Connect(wsURL))
	defer client.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, client.ExecuteCodeStream("print('hi')", resultChan))

	start := time.Now()
	sawStdout, sawTerminal := false, false
	for result := range resultChan {
		if result == nil {
			break
		}
		if len(result.Stream) == 1 && result.Stream[0].Text == "hi\n" {
			sawStdout = true
		}
		if result.ExecutionTime > 0 && result.Error == nil && len(result.Stream) == 0 {
			sawTerminal = true
		}
	}
	elapsed := time.Since(start)

	require.True(t, sawStdout, "expected stdout before the terminal result")
	require.True(t, sawTerminal, "expected the terminal complete result")
	require.Less(t, elapsed, time.Second, "idle must finalize the stream without waiting for the reply deadline")
}
