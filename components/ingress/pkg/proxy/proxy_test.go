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

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubNoSecureProvider struct{}

func (stubNoSecureProvider) GetEndpoint(string) (*sandbox.EndpointInfo, error) {
	return &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}, nil
}

func (p stubNoSecureProvider) ResolveEndpoint(_ context.Context, target sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	return p.GetEndpoint(target.SandboxID)
}

func (stubNoSecureProvider) Start(context.Context) error { return nil }

func TestIsWebSocketRequest(t *testing.T) {
	proxy := &Proxy{}

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	// RFC 7230 §6.1 permits Connection token lists emitted by L7 proxies.
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive, Upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "WebSocket")
	req.Header.Set("Connection", "upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	assert.False(t, proxy.isWebSocketRequest(req))

	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive")
	assert.False(t, proxy.isWebSocketRequest(req))

	// POST with upgrade headers is not a WebSocket handshake.
	req = httptest.NewRequest(http.MethodPost, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	assert.False(t, proxy.isWebSocketRequest(req))
}

// The WebSocket acceptor requires an HTTP/1.1 GET upgrade, not RFC 8441 CONNECT.
func TestIsWebSocketRequestRejectsH2ExtendedConnect(t *testing.T) {
	proxy := &Proxy{}

	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.ProtoMajor = 2
	req.Header.Set(":protocol", "websocket")
	assert.False(t, proxy.isWebSocketRequest(req))

	req = httptest.NewRequest(http.MethodConnect, "example.com:443", nil)
	req.ProtoMajor = 2
	assert.False(t, proxy.isWebSocketRequest(req))
}

func TestParseHostRoute(t *testing.T) {
	pr, err := parseHostRoute("sandbox-1234.example.com")
	assert.NoError(t, err)
	assert.Equal(t, "sandbox", pr.sandboxID)
	assert.Equal(t, 1234, pr.port)

	pr, err = parseHostRoute("https://alpha-beta-8080.sandbox.test")
	assert.NoError(t, err)
	assert.Equal(t, "alpha-beta", pr.sandboxID)
	assert.Equal(t, 8080, pr.port)

	_, err = parseHostRoute("invalidhost")
	assert.Error(t, err)

	_, err = parseHostRoute("-1234.example.com")
	assert.Error(t, err)
}

func TestGetSandboxHostRejectsZeroPortWithStableError(t *testing.T) {
	proxy := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "http://myhost-0.example.com/", nil)
	_, status, err := proxy.getSandboxHostDefinition(request)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.EqualError(t, err, `invalid ingress route: invalid port format: invalid port "0"`)
}

func TestServeHTTPRejectsInvalidHostRoutePort(t *testing.T) {
	proxy := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
	for _, host := range []string{"myhost-70000.example.com", "myhost-08080.example.com", "my-host-08080.example.com"} {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code, host)
	}
}

func TestServeHTTPStreamsResponseWhileRequestBodyIsSent(t *testing.T) {
	const head, tail, started = "first part,", " second part", "started\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Like a sandbox process that streams output while still reading its
		// input, answer before the request body ends.
		controller := http.NewResponseController(w)
		if err := controller.EnableFullDuplex(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		received := make([]byte, len(head))
		if _, err := io.ReadFull(r.Body, received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, started)
		if err := controller.Flush(); err != nil {
			return
		}
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "received %s%s\n", received, rest)
	}))
	defer upstream.Close()
	ingress := httptest.NewServer(NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil))
	defer ingress.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The body has no declared length and ends only after the client has
	// read the start of the response.
	responseStarted := make(chan struct{})
	body, bodyWriter := io.Pipe()
	go func() {
		_, _ = io.WriteString(bodyWriter, head)
		select {
		case <-responseStarted:
			_, _ = io.WriteString(bodyWriter, tail)
			_ = bodyWriter.Close()
		case <-ctx.Done():
			_ = bodyWriter.CloseWithError(ctx.Err())
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ingress.URL+"/run", body)
	require.NoError(t, err)
	request.Header.Set(SandboxIngress, fmt.Sprintf("sandbox-%d", upstream.Listener.Addr().(*net.TCPAddr).Port))

	response, err := ingress.Client().Do(request)
	require.NoError(t, err, "the response must start while the request body is still being sent")
	defer response.Body.Close()
	first := make([]byte, len(started))
	_, err = io.ReadFull(response.Body, first)
	require.NoError(t, err)
	require.Equal(t, started, string(first))
	close(responseStarted)
	rest, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "received "+head+tail+"\n", string(rest))
}

func TestServeHTTPAnnouncesCloseWhenRequestBodyIsLeftUnread(t *testing.T) {
	ingress := httptest.NewServer(NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil))
	defer ingress.Close()

	// The requests name no sandbox, so the gateway answers 400 without
	// reading their bodies.
	for _, tc := range []struct {
		name   string
		header string // the header that frames the body
		sent   string // the part of the body sent before the response is read
		rest   string // the part sent after it
		closes bool
	}{
		{name: "declared length, 300 KiB unread", header: "Content-Length: 307200", closes: true},
		{name: "declared length, 100 KiB unread", header: "Content-Length: 102400", sent: strings.Repeat("x", 102400)},
		{name: "chunked, unfinished", header: "Transfer-Encoding: chunked", sent: "5\r\nhello\r\n", rest: "0\r\n\r\n", closes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", ingress.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = io.WriteString(conn, "POST /run HTTP/1.1\r\nHost: gateway\r\n"+tc.header+"\r\n\r\n"+tc.sent)
			require.NoError(t, err)
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			require.Equal(t, tc.closes, response.Close, "whether the response announces Connection: close")

			_, err = io.WriteString(conn, tc.rest)
			require.NoError(t, err)
			if tc.closes {
				_, err = reader.ReadByte()
				require.ErrorIs(t, err, io.EOF, "the server closes the connection after the response")
				return
			}
			// The server discards the rest of the body and answers the next
			// request on the same connection.
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: gateway\r\n\r\n")
			require.NoError(t, err)
			next, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
			require.NoError(t, err)
			require.NoError(t, next.Body.Close())
			require.Equal(t, http.StatusBadRequest, next.StatusCode)
		})
	}
}

func TestGetClientIP(t *testing.T) {
	proxy := &Proxy{}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	assert.Equal(t, "192.0.2.1", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(xRealIP, "203.0.113.5")
	assert.Equal(t, "203.0.113.5", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(xForwardedFor, "10.0.0.1, 198.51.100.2")
	assert.Equal(t, "10.0.0.1", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(xForwardedFor, "10.0.0.1,198.51.100.2")
	assert.Equal(t, "10.0.0.1", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(xForwardedFor, " 10.0.0.1 ")
	assert.Equal(t, "10.0.0.1", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(xForwardedFor, ", 198.51.100.2")
	req.Header.Set(xRealIP, "203.0.113.5")
	assert.Equal(t, "203.0.113.5", proxy.getClientIP(req))
}

func findAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	return port, nil
}

func TestStatusCapturingResponseWriterRecordsFinalStatusAfterEarlyHints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	assert.NoError(t, err)

	sw := &statusCapturingResponseWriter{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(sw, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusNotFound, sw.statusCode)
	assert.True(t, sw.written)
}

func TestStatusCapturingResponseWriterInformationalStatusDoesNotCommit(t *testing.T) {
	for _, code := range []int{http.StatusContinue, http.StatusEarlyHints} {
		sw := &statusCapturingResponseWriter{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
		sw.WriteHeader(code)
		assert.False(t, sw.written, "status %d", code)
		assert.Equal(t, http.StatusOK, sw.statusCode, "status %d", code)
	}

	// 101 is the final response of a protocol upgrade.
	sw := &statusCapturingResponseWriter{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
	sw.WriteHeader(http.StatusSwitchingProtocols)
	assert.True(t, sw.written)
	assert.Equal(t, http.StatusSwitchingProtocols, sw.statusCode)
}

type countingProvider struct {
	calls int
}

func (c *countingProvider) GetEndpoint(string) (*sandbox.EndpointInfo, error) {
	c.calls++
	return &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}, nil
}

func (c *countingProvider) ResolveEndpoint(_ context.Context, target sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	return c.GetEndpoint(target.SandboxID)
}

func (c *countingProvider) Start(context.Context) error { return nil }

func TestServeHTTPBlocksExecdInternalInitOnPort44772(t *testing.T) {
	blockedPaths := []string{
		"/internal/init",
		"/internal/init/",
		"/internal/init/extra",
		"//internal//init",
		"/foo/../internal/init",
		"/internal/./init",
		"/INTERNAL/INIT",
		"/internal%2Finit",
		"/%69nternal/init",
	}

	t.Run("header mode blocks without resolving endpoint", func(t *testing.T) {
		provider := &countingProvider{}
		p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil)

		for _, rawPath := range blockedPaths {
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, "http://sandbox-44772.example.com"+rawPath, nil)
				p.ServeHTTP(rec, req)
				require.Equal(t, http.StatusForbidden, rec.Code, "method=%s path=%s", method, rawPath)
				require.Contains(t, rec.Body.String(), "/internal/init")
			}
		}
		require.Zero(t, provider.calls, "blocked /internal/init must not resolve or wake sandbox")
	})

	t.Run("uri mode blocks without resolving endpoint", func(t *testing.T) {
		provider := &countingProvider{}
		p := NewProxy(context.Background(), provider, ModeURI, nil, nil, nil)

		for _, rawPath := range blockedPaths {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://ingress.local/sandbox/44772"+rawPath, nil)
			p.ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "path=%s", rawPath)
		}
		require.Zero(t, provider.calls, "blocked /internal/init must not resolve or wake sandbox")
	})

	t.Run("allows /internal/init on non-execd ports and other paths on 44772", func(t *testing.T) {
		provider := &countingProvider{}
		pHeader := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil)

		for _, targetURL := range []string{
			"http://sandbox-8080.example.com/internal/init",
			"http://sandbox-44772.example.com/ping",
			"http://sandbox-44772.example.com/ready",
			"http://sandbox-44772.example.com/command",
			"http://sandbox-44772.example.com/internal/initialize",
		} {
			req := httptest.NewRequest(http.MethodPost, targetURL, nil)
			host, status, err := pHeader.getSandboxHostDefinition(req)
			require.NoError(t, err, targetURL)
			require.Zero(t, status, targetURL)
			require.NotNil(t, host, targetURL)
		}

		pURI := NewProxy(context.Background(), provider, ModeURI, nil, nil, nil)
		req := httptest.NewRequest(http.MethodPost, "http://ingress.local/sandbox/8080/internal/init", nil)
		host, status, err := pURI.getSandboxHostDefinition(req)
		require.NoError(t, err)
		require.Zero(t, status)
		require.NotNil(t, host)
	})
}
