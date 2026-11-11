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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func browserPTYProtocols(token string) string {
	return model.PTYWebSocketProtocol + ", " + model.PTYWebSocketTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(token))
}

func TestPTYSubprotocolAuthentication(t *testing.T) {
	const token = "a/token+with=separators"
	credential := model.PTYWebSocketTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(token))
	for _, useBinding := range []bool{false, true} {
		mode := "startup-token"
		var runtimeBinding *binding.RuntimeBinding
		if useBinding {
			mode = "runtime-binding"
			runtimeBinding = &binding.RuntimeBinding{HasAccessToken: true, AccessTokenHash: mustHash(t, token)}
		}
		t.Run(mode, func(t *testing.T) {
			withTestBinding(t, runtimeBinding)
			legacyToken := token
			if useBinding {
				legacyToken = "superseded-startup-token"
			}
			tests := []struct {
				name      string
				method    string
				path      string
				protocols []string
				header    *string
				upgrade   bool
				want      int
			}{
				{name: "browser token", protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 200},
				{name: "multiple header lines", protocols: []string{model.PTYWebSocketProtocol, credential}, upgrade: true, want: 200},
				{name: "viewer", path: "/pty/p1/ws?mode=viewer", protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 200},
				{name: "missing token", upgrade: true, want: 401},
				{name: "wrong token", protocols: []string{browserPTYProtocols("wrong")}, upgrade: true, want: 401},
				{name: "empty token", protocols: []string{browserPTYProtocols("")}, upgrade: true, want: 401},
				{name: "invalid base64", protocols: []string{model.PTYWebSocketProtocol + ", " + model.PTYWebSocketTokenPrefix + "%%%"}, upgrade: true, want: 401},
				{name: "padded base64", protocols: []string{model.PTYWebSocketProtocol + ", " + model.PTYWebSocketTokenPrefix + "dA=="}, upgrade: true, want: 401},
				{name: "noncanonical base64", protocols: []string{model.PTYWebSocketProtocol + ", " + model.PTYWebSocketTokenPrefix + "dB"}, upgrade: true, want: 401},
				{name: "duplicate token", protocols: []string{browserPTYProtocols(token), credential}, upgrade: true, want: 401},
				{name: "conflicting tokens", protocols: []string{browserPTYProtocols(token), model.PTYWebSocketTokenPrefix + "dA"}, upgrade: true, want: 401},
				{name: "missing application protocol", protocols: []string{credential}, upgrade: true, want: 401},
				{name: "header only unchanged", header: stringPointer(token), upgrade: true, want: 200},
				{name: "header wins", header: stringPointer(token), protocols: []string{browserPTYProtocols("wrong")}, upgrade: true, want: 200},
				{name: "bad header cannot fall back", header: stringPointer("wrong"), protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 401},
				{name: "empty header cannot fall back", header: stringPointer(""), protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 401},
				{name: "malformed token rejected with valid header", header: stringPointer(token), protocols: []string{browserPTYProtocols(token), credential}, upgrade: true, want: 401},
				{name: "not an upgrade", protocols: []string{browserPTYProtocols(token)}, want: 401},
				{name: "not a PTY route", path: "/command", protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 401},
				{name: "PTY create needs header", method: http.MethodPost, path: "/pty", protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 401},
				{name: "PTY delete needs header", method: http.MethodDelete, path: "/pty/p1", protocols: []string{browserPTYProtocols(token)}, upgrade: true, want: 401},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					r := gin.New()
					r.Use(accessTokenMiddleware(legacyToken))
					handler := func(ctx *gin.Context) {
						if ctx.FullPath() == "/pty/:sessionId/ws" && tt.upgrade {
							for _, value := range ctx.Request.Header.Values("Sec-WebSocket-Protocol") {
								require.NotContains(t, value, model.PTYWebSocketTokenPrefix)
							}
						}
						ctx.Status(http.StatusOK)
					}
					r.GET("/pty/:sessionId/ws", handler)
					r.GET("/command", handler)
					r.POST("/pty", handler)
					r.DELETE("/pty/:sessionId", handler)
					method, path := tt.method, tt.path
					if method == "" {
						method = http.MethodGet
					}
					if path == "" {
						path = "/pty/p1/ws"
					}
					req := httptest.NewRequest(method, path, nil)
					for _, protocol := range tt.protocols {
						req.Header.Add("Sec-WebSocket-Protocol", protocol)
					}
					if tt.header != nil {
						req.Header.Set(model.ApiAccessTokenHeader, *tt.header)
					}
					if tt.upgrade {
						req.Header.Set("Connection", "Upgrade")
						req.Header.Set("Upgrade", "websocket")
					}
					rec := httptest.NewRecorder()
					r.ServeHTTP(rec, req)
					require.Equal(t, tt.want, rec.Code)
					require.NotContains(t, rec.Body.String(), token)
					require.NotContains(t, rec.Body.String(), credential)
				})
			}
		})
	}
}

func TestPTYSubprotocolDoesNotEnableAuthentication(t *testing.T) {
	withTestBinding(t, nil)
	r := gin.New()
	r.Use(accessTokenMiddleware(""))
	r.GET("/pty/:sessionId/ws", func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	for _, protocols := range []string{"", browserPTYProtocols("arbitrary")} {
		req := httptest.NewRequest(http.MethodGet, "/pty/p1/ws", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Protocol", protocols)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}
}

func stringPointer(value string) *string { return &value }
