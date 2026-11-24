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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProxyMiddlewareReturnsSidecarForbiddenForActiveVault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receivedPath := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath <- r.URL.Path
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	router := gin.New()
	router.Use(ProxyMiddleware())
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/proxy/"+port+"/credential-vault/_active", nil)
	require.NoError(t, err)
	resp, err := proxyServer.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "/credential-vault/_active", <-receivedPath)
}

func TestProxyMiddlewareReusesUpstreamConnections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var newConns atomic.Int32
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	backend.Start()
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	router := gin.New()
	router.Use(ProxyMiddleware())
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	const requests = 5
	for range requests {
		resp, err := proxyServer.Client().Get(proxyServer.URL + "/proxy/" + port + "/")
		require.NoError(t, err)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	require.Equal(t, int32(1), newConns.Load(),
		"sequential proxied requests should reuse one keep-alive upstream connection")
}
