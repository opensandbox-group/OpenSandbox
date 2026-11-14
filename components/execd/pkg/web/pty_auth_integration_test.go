//go:build !windows

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

package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestPTYBrowserWebSocketHandshake(t *testing.T) {
	const token = "browser/access+token="
	withRuntimeInit(t, false)
	withTestBinding(t, &binding.RuntimeBinding{HasAccessToken: true, AccessTokenHash: mustHash(t, token)})
	runner := controller.InitCodeRunner()
	execd := httptest.NewServer(NewRouter("obsolete-startup-token"))
	t.Cleanup(execd.Close)
	// Exercise an HTTP Upgrade proxy as well as the real execd router. Actual
	// deployment ingress and browser integration are separate acceptance checks.
	target, err := url.Parse(execd.URL)
	require.NoError(t, err)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)

	createSession := func() string {
		req, err := http.NewRequest(http.MethodPost, proxy.URL+"/pty", strings.NewReader(`{"command":"/bin/sh"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(model.ApiAccessTokenHeader, token)
		resp, err := proxy.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var created model.CreatePTYSessionResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
		require.NotEmpty(t, created.SessionID)
		t.Cleanup(func() { _ = runner.DeletePTYSession(created.SessionID) })
		return created.SessionID
	}
	credential := model.PTYWebSocketTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(token))
	protocols := []string{model.PTYWebSocketProtocol, credential}
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http")
	id := createSession()
	for _, offered := range [][]string{
		nil,
		{model.PTYWebSocketProtocol, model.PTYWebSocketTokenPrefix + "d3Jvbmc"},
		{model.PTYWebSocketProtocol, credential, credential},
		{credential},
	} {
		dialer := websocket.Dialer{Subprotocols: offered, HandshakeTimeout: 3 * time.Second}
		_, resp, err := dialer.Dial(wsURL+"/pty/"+id+"/ws", nil)
		require.Error(t, err)
		require.NotNil(t, resp)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		_ = resp.Body.Close()
		running, offset, err := runner.GetPTYSessionStatus(id)
		require.NoError(t, err)
		require.False(t, running, "rejected handshake must not start the shell")
		require.Zero(t, offset, "rejected handshake must not produce replay output")
	}

	dial := func(id, query string, offered []string, headers http.Header) *websocket.Conn {
		dialer := websocket.Dialer{Subprotocols: offered, HandshakeTimeout: 3 * time.Second}
		conn, resp, err := dialer.Dial(wsURL+"/pty/"+id+"/ws"+query, headers)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.NotContains(t, resp.Header.Get("Sec-WebSocket-Protocol"), credential)
		if len(offered) > 0 {
			require.Equal(t, model.PTYWebSocketProtocol, conn.Subprotocol())
		} else {
			require.Empty(t, conn.Subprotocol())
		}
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
		for {
			kind, data, err := conn.ReadMessage()
			require.NoError(t, err)
			if kind != websocket.TextMessage {
				continue
			}
			var frame model.ServerFrame
			require.NoError(t, json.Unmarshal(data, &frame))
			if frame.Type == "connected" {
				return conn
			}
		}
	}

	holder := dial(id, "", protocols, nil)
	viewer := dial(id, "?mode=viewer", protocols, nil)
	require.NoError(t, holder.WriteJSON(model.ClientFrame{Type: "resize", Cols: 100, Rows: 35}))
	require.NoError(t, holder.WriteMessage(websocket.BinaryMessage, append([]byte{model.BinStdin}, []byte("printf browser-auth-ok\\n\n")...)))
	for _, conn := range []*websocket.Conn{holder, viewer} {
		var output strings.Builder
		for !strings.Contains(output.String(), "browser-auth-ok") {
			kind, data, err := conn.ReadMessage()
			require.NoError(t, err)
			if kind == websocket.BinaryMessage && len(data) > 0 {
				offset := 1
				if data[0] == model.BinReplay {
					offset = 9
				}
				if len(data) > offset {
					output.Write(data[offset:])
				}
			}
		}
	}

	// Header-only SDK/CLI clients still attach without any subprotocol.
	dial(createSession(), "", nil, http.Header{model.ApiAccessTokenHeader: []string{token}})
	// An explicitly valid header wins over a different, well-formed protocol token.
	dial(createSession(), "", []string{model.PTYWebSocketProtocol, model.PTYWebSocketTokenPrefix + "d3Jvbmc"},
		http.Header{model.ApiAccessTokenHeader: []string{token}})
}
