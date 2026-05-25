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
// +build !windows

package controller

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// buildPTYRouter assembles a minimal Gin router with only the /pty routes,
// avoiding any import cycle with pkg/web.
func buildPTYRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())

	pty := r.Group("/pty")
	{
		pty.POST("", func(ctx *gin.Context) {
			NewPTYController(ctx).CreatePTYSession()
		})
		pty.GET("/:sessionId", func(ctx *gin.Context) {
			NewPTYController(ctx).GetPTYSessionStatus()
		})
		pty.DELETE("/:sessionId", func(ctx *gin.Context) {
			NewPTYController(ctx).DeletePTYSession()
		})
		pty.GET("/:sessionId/ws", PTYSessionWebSocket)
	}
	return r
}

// newPTYTestServer creates a test HTTP server with fresh codeRunner and PTY routes only.
func newPTYTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	prev := codeRunner
	codeRunner = runtime.NewController("", "")
	t.Cleanup(func() { codeRunner = prev })
	return httptest.NewServer(buildPTYRouter())
}

// wsDialPTY dials a WebSocket URL; accepts an optional extra query string.
func wsDialPTY(t *testing.T, baseURL, path, query string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(baseURL+path, "http")
	if query != "" {
		u += "?" + query
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("WS dial %s: %v (HTTP %d)", u, err, resp.StatusCode)
		}
		t.Fatalf("WS dial %s: %v", u, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// wsDialExpectHTTP dials and returns the HTTP status without upgrading (for 4xx cases).
func wsDialExpectHTTP(t *testing.T, baseURL, path, query string) int {
	t.Helper()
	u := "ws" + strings.TrimPrefix(baseURL+path, "http")
	if query != "" {
		u += "?" + query
	}
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	require.Error(t, err, "expected dial to fail with HTTP error")
	require.NotNil(t, resp)
	return resp.StatusCode
}

// ptyCreateSession calls POST /pty and returns the session_id.
func ptyCreateSession(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/pty", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var r model.CreatePTYSessionResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
	require.NotEmpty(t, r.SessionID)
	return r.SessionID
}

// ptyReadFrame reads the next frame, handling both binary data frames and JSON control frames.
func ptyReadFrame(conn *websocket.Conn, timeout time.Duration) (model.ServerFrame, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	msgType, raw, err := conn.ReadMessage()
	if err != nil {
		return model.ServerFrame{}, err
	}
	if msgType == websocket.TextMessage {
		var f model.ServerFrame
		return f, json.Unmarshal(raw, &f)
	}
	// Binary data frame: decode type byte into ServerFrame.
	if len(raw) == 0 {
		return model.ServerFrame{}, errors.New("empty binary frame")
	}
	switch raw[0] {
	case model.BinStdout:
		return model.ServerFrame{Type: "stdout", Data: string(raw[1:])}, nil
	case model.BinStderr:
		return model.ServerFrame{Type: "stderr", Data: string(raw[1:])}, nil
	case model.BinReplay:
		if len(raw) < 9 {
			return model.ServerFrame{}, fmt.Errorf("replay frame too short: %d bytes", len(raw))
		}
		offset := int64(binary.BigEndian.Uint64(raw[1:9]))
		return model.ServerFrame{Type: "replay", Data: string(raw[9:]), Offset: offset}, nil
	}
	return model.ServerFrame{}, fmt.Errorf("unknown binary frame type 0x%02x", raw[0])
}

// ptyWaitFrame reads frames until one with the given type is found.
func ptyWaitFrame(t *testing.T, conn *websocket.Conn, wantType string, timeout time.Duration) model.ServerFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := ptyReadFrame(conn, time.Until(deadline))
		if err != nil {
			t.Fatalf("ptyWaitFrame(%q): %v", wantType, err)
		}
		if f.Type == wantType {
			return f
		}
	}
	t.Fatalf("ptyWaitFrame(%q): timed out after %s", wantType, timeout)
	return model.ServerFrame{}
}

// ptyOutputContains reads frames until stdout/stderr/replay contains substr.
func ptyOutputContains(t *testing.T, conn *websocket.Conn, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := ptyReadFrame(conn, time.Until(deadline))
		if err != nil {
			t.Fatalf("ptyOutputContains(%q): read error: %v", substr, err)
		}
		if f.Type == "stdout" || f.Type == "stderr" || f.Type == "replay" {
			if strings.Contains(f.Data, substr) {
				return
			}
		}
	}
	t.Fatalf("ptyOutputContains(%q): timed out", substr)
}

// ptyWriteStdin sends a binary stdin frame (production path).
func ptyWriteStdin(t *testing.T, conn *websocket.Conn, text string) {
	t.Helper()
	frame := make([]byte, 1+len(text))
	frame[0] = model.BinStdin
	copy(frame[1:], text)
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, frame))
}

// --- Tests ---

func TestPTYWS_UnknownSessionReturns404(t *testing.T) {
	srv := newPTYTestServer(t)
	defer srv.Close()

	code := wsDialExpectHTTP(t, srv.URL, "/pty/nonexistent/ws", "")
	require.Equal(t, http.StatusNotFound, code)
}

func TestPTYWS_AlreadyConnectedReturns409(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn1 := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn1, "connected", 10*time.Second)

	code := wsDialExpectHTTP(t, srv.URL, "/pty/"+id+"/ws", "")
	require.Equal(t, http.StatusConflict, code)
}

func TestPTYWS_ConnectedFramePTYMode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	f := ptyWaitFrame(t, conn, "connected", 10*time.Second)

	require.Equal(t, "connected", f.Type)
	require.Equal(t, id, f.SessionID)
	require.Equal(t, "pty", f.Mode)
}

func TestPTYWS_StdinForwarding(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn, "connected", 10*time.Second)

	ptyWriteStdin(t, conn, "echo hello_ws\n")
	ptyOutputContains(t, conn, "hello_ws", 8*time.Second)
}

func TestPTYWS_PingPong(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn, "connected", 10*time.Second)

	require.NoError(t, conn.WriteJSON(model.ClientFrame{Type: "ping"}))
	f := ptyWaitFrame(t, conn, "pong", 5*time.Second)
	require.Equal(t, "pong", f.Type)
}

func TestPTYWS_ExitFrame(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn, "connected", 10*time.Second)

	ptyWriteStdin(t, conn, "exit 0\n")

	f := ptyWaitFrame(t, conn, "exit", 10*time.Second)
	require.Equal(t, "exit", f.Type)
	require.NotNil(t, f.ExitCode)
	require.Equal(t, 0, *f.ExitCode)
}

func TestPTYWS_ReplayOnReconnect(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)

	// First connection: produce output.
	conn1 := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn1, "connected", 10*time.Second)
	ptyWriteStdin(t, conn1, "echo replay_test\n")
	ptyOutputContains(t, conn1, "replay_test", 8*time.Second)

	// Check offset via REST.
	resp, err := http.Get(srv.URL + "/pty/" + id)
	require.NoError(t, err)
	defer resp.Body.Close()
	var status model.PTYSessionStatusResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&status))
	require.True(t, status.OutputOffset > 0)

	// Disconnect.
	_ = conn1.Close()
	time.Sleep(100 * time.Millisecond)

	// Reconnect from offset 0 — should receive a replay frame.
	conn2 := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "since=0")

	deadline := time.Now().Add(8 * time.Second)
	gotReplay := false
	for time.Now().Before(deadline) {
		f, err2 := ptyReadFrame(conn2, time.Until(deadline))
		if err2 != nil {
			break
		}
		if f.Type == "replay" && strings.Contains(f.Data, "replay_test") {
			gotReplay = true
			break
		}
	}
	require.True(t, gotReplay, "expected replay frame containing 'replay_test'")
}

func TestPTYWS_ResizeFrame(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "")
	ptyWaitFrame(t, conn, "connected", 10*time.Second)

	require.NoError(t, conn.WriteJSON(model.ClientFrame{
		Type: "resize",
		Cols: 120,
		Rows: 40,
	}))
	time.Sleep(100 * time.Millisecond)

	ptyWriteStdin(t, conn, "stty size\n")
	ptyOutputContains(t, conn, "40 120", 8*time.Second)
}

func TestPTYWS_PipeModeConnectedFrame(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)
	conn := wsDialPTY(t, srv.URL, "/pty/"+id+"/ws", "pty=0")

	f := ptyWaitFrame(t, conn, "connected", 10*time.Second)
	require.Equal(t, "pipe", f.Mode)
}

func TestPTYWS_RESTGetStatus(t *testing.T) {
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)

	resp, err := http.Get(srv.URL + "/pty/" + id)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var s model.PTYSessionStatusResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&s))
	require.Equal(t, id, s.SessionID)
	require.False(t, s.Running)
}

func TestPTYWS_RESTDeleteSession(t *testing.T) {
	srv := newPTYTestServer(t)
	defer srv.Close()

	id := ptyCreateSession(t, srv)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/pty/"+id, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/pty/"+id, nil)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	_ = resp2.Body.Close()
	require.Equal(t, http.StatusNotFound, resp2.StatusCode)
}
