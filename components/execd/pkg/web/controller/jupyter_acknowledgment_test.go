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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestRunCodeEndsWhenKernelDoesNotAcknowledge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/kernelspecs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kernelspecs":{"opensandbox-python":{"spec":{"language":"python"}}}}`))
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"test-session","kernel":{"id":"test-kernel"}}`))
	})
	mux.HandleFunc("/api/kernels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"test-kernel"}]`))
	})
	requests := make(chan string, 2)
	mux.HandleFunc("/api/kernels/test-kernel/channels", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()

		var msg execute.Message
		var request execute.ExecuteRequest
		if err := conn.ReadJSON(&msg); err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal(msg.Content, &request); err != nil || msg.Header.MessageType != string(execute.MsgExecuteRequest) {
			t.Errorf("expected execute_request, got %+v (decode error: %v)", msg, err)
			return
		}
		requests <- request.Code

		send := func(kind execute.MessageType, parent execute.Header, content string) bool {
			if err := conn.WriteJSON(execute.Message{
				Header:       execute.Header{MessageType: string(kind)},
				ParentHeader: parent,
				Content:      json.RawMessage(content),
			}); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
		if request.Code == "print(1)" {
			// Leave the socket open and send only traffic belonging to another request.
			if !send(execute.MsgStatus, execute.Header{MessageID: "unrelated"}, `{"execution_state":"busy"}`) {
				return
			}
		} else {
			if !send(execute.MsgStatus, msg.Header, `{"execution_state":"busy"}`) {
				return
			}
			// An acknowledged execution can be silent past the five-second deadline.
			select {
			case <-time.After(5500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			if !send(execute.MsgStream, msg.Header, `{"name":"stdout","text":"2\n"}`) ||
				!send(execute.MsgExecuteReply, msg.Header, `{"status":"ok","execution_count":1}`) ||
				!send(execute.MsgStatus, msg.Header, `{"execution_state":"idle"}`) {
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	runner := runtime.NewController(server.URL, "test-token")
	sessionID, err := runner.CreateContext(ctx, &runtime.CreateContextRequest{Language: runtime.Python})
	require.NoError(t, err)
	previousRunner := codeRunner
	codeRunner = runner
	t.Cleanup(func() { codeRunner = previousRunner })

	run := func(code string) string {
		body := fmt.Appendf(nil, `{"code":%q,"context":{"id":%q,"language":"python"}}`, code, sessionID)
		ginCtx, recorder := newTestContext(http.MethodPost, "/code", body)
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			NewCodeInterpretingController(ginCtx).RunCode()
		}()
		select {
		case <-requests:
		case <-done:
			t.Fatalf("POST /code returned before sending execute_request: %s", recorder.Body.String())
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("Jupyter fixture did not receive execute_request")
		}
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			elapsed := time.Since(start)
			cancel()
			<-done
			t.Fatalf("POST /code stayed open for %s after an unacknowledged execute_request", elapsed.Round(time.Millisecond))
		}
		t.Logf("POST /code for %s returned after %s", code, time.Since(start).Round(time.Millisecond))
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
		return recorder.Body.String()
	}

	failed := run("print(1)")
	require.Contains(t, failed, `"type":"init"`)
	require.Equal(t, 1, strings.Count(failed, `"type":"error"`))
	require.Contains(t, failed, "KernelAcknowledgmentTimeout")
	require.NotContains(t, failed, `"type":"execution_complete"`)

	succeeded := run("print(2)")
	require.Contains(t, succeeded, `"type":"stdout","text":"2\n"`)
	require.Contains(t, succeeded, `"type":"execution_complete"`)
	require.NotContains(t, succeeded, `"type":"error"`)
}
