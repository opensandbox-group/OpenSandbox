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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_WebSocketProxy(t *testing.T) {
	t.Run("with header mode", func(t *testing.T) {
		webSocketProxyWithHeaderMode(t)
	})
	t.Run("with uri mode", func(t *testing.T) {
		webSocketProxyWithURIMode(t)
	})
}

func webSocketProxyWithHeaderMode(t *testing.T) {
	provider := &mockProvider{
		endpoints: map[string]string{
			"test-sandbox": "127.0.0.1",
		},
	}

	ctx := context.Background()
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})
	proxy := NewProxy(ctx, provider, ModeHeader, nil, nil, nil)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	proxyPort, err := findAvailablePort()
	require.NoError(t, err)
	proxyURL := "ws://127.0.0.1:" + strconv.Itoa(proxyPort)

	go func() {
		assert.NoError(t, http.ListenAndServe(":"+strconv.Itoa(proxyPort), mux))
	}()

	time.Sleep(2 * time.Second)

	backendPort, err := findAvailablePort()
	require.NoError(t, err)

	go func() {
		mux2 := http.NewServeMux()
		mux2.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			// Backend must see the original virtual-host header, otherwise
			// vhost routing at the sandbox side breaks.
			assert.True(t, strings.HasPrefix(r.Host, "127.0.0.1"))

			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				log.Println(err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			conn.SetReadLimit(defaultWebSocketMessageSizeLimit)

			msgType, msg, readErr := conn.Read(context.Background())
			if readErr != nil {
				return
			}
			_ = conn.Write(context.Background(), msgType, msg)
		})
		if err := http.ListenAndServe(":"+strconv.Itoa(backendPort), mux2); err != nil {
			t.Error("ListenAndServe: ", err)
		}
	}()

	time.Sleep(time.Millisecond * 100)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	msg := "hello kite"
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(msg)))

	msgType, got, err := conn.Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, msgType)
	assert.Equal(t, msg, string(got))
}

func webSocketProxyWithURIMode(t *testing.T) {
	provider := &mockProvider{
		endpoints: map[string]string{
			"test-sandbox": "127.0.0.1",
		},
	}

	ctx := context.Background()
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})
	proxy := NewProxy(ctx, provider, ModeURI, nil, nil, nil)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	proxyPort, err := findAvailablePort()
	require.NoError(t, err)
	proxyURL := "ws://127.0.0.1:" + strconv.Itoa(proxyPort)

	go func() {
		assert.NoError(t, http.ListenAndServe(":"+strconv.Itoa(proxyPort), mux))
	}()

	time.Sleep(2 * time.Second)

	backendPort, err := findAvailablePort()
	require.NoError(t, err)

	go func() {
		mux2 := http.NewServeMux()
		mux2.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			assert.True(t, strings.HasPrefix(r.Host, "127.0.0.1"))

			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				log.Println(err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			conn.SetReadLimit(defaultWebSocketMessageSizeLimit)

			msgType, msg, readErr := conn.Read(context.Background())
			if readErr != nil {
				return
			}
			_ = conn.Write(context.Background(), msgType, msg)
		})
		if err := http.ListenAndServe(":"+strconv.Itoa(backendPort), mux2); err != nil {
			t.Error("ListenAndServe: ", err)
		}
	}()

	time.Sleep(time.Millisecond * 100)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, _, err := websocket.Dial(
		context.Background(),
		proxyURL+fmt.Sprintf("/test-sandbox/%v", backendPort)+"/ws",
		&websocket.DialOptions{HTTPHeader: h},
	)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	msg := "hello kite"
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(msg)))

	msgType, got, err := conn.Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, msgType)
	assert.Equal(t, msg, string(got))
}

// startProxyForBehaviorTest wires a proxy in front of an in-process backend
// exposed on a free port. It returns the proxy URL and the backend port so the
// test can shape the sandbox route header.
//
// This helper intentionally does not reset the package-level Logger. Sister
// tests already initialize it; overwriting the pointer here would race with a
// proxy goroutine still reading it as the previous test's servers wind down.
func startProxyForBehaviorTest(t *testing.T, backendMux *http.ServeMux, opts ...Option) (proxyURL string, backendPort int) {
	t.Helper()

	provider := &mockProvider{endpoints: map[string]string{"test-sandbox": "127.0.0.1"}}
	proxy := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil, opts...)

	proxyPort, err := findAvailablePort()
	require.NoError(t, err)
	backendPort, err = findAvailablePort()
	require.NoError(t, err)

	backendSrv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", backendPort), Handler: backendMux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = backendSrv.ListenAndServe() }()
	t.Cleanup(func() { _ = backendSrv.Close() })

	proxyMux := http.NewServeMux()
	proxyMux.Handle("/", proxy)
	proxySrv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", proxyPort), Handler: proxyMux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = proxySrv.ListenAndServe() }()
	t.Cleanup(func() { _ = proxySrv.Close() })

	// Poll the proxy listener rather than sleeping: fast when the OS wires
	// the socket immediately, still bounded when a slow scheduler delays it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status.ok", proxyPort)); err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	return "ws://127.0.0.1:" + strconv.Itoa(proxyPort), backendPort
}

// Test_WebSocketProxy_Subprotocol asserts that the client's Sec-WebSocket-
// Protocol offer is forwarded to the backend and the backend's selection is
// echoed back to the client. Gorilla's proxy handled this implicitly; the
// coder/websocket migration required explicit wiring in DialOptions and
// AcceptOptions and this test guards the wiring from silent regression.
func Test_WebSocketProxy_Subprotocol(t *testing.T) {
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			Subprotocols:       []string{"graphql-ws"},
		})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, _ = conn.Read(r.Context())
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{
		HTTPHeader:   h,
		Subprotocols: []string{"graphql-ws", "graphql-transport-ws"},
	})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	assert.Equal(t, "graphql-ws", conn.Subprotocol(),
		"backend selected graphql-ws; proxy must echo it to the client")
}

// Test_WebSocketProxy_SetCookieForwarded asserts that Set-Cookie headers the
// backend sets during the WebSocket handshake reach the client. code-server
// refreshes session cookies at upgrade time and dropping them silently breaks
// sticky-session logins on some deployments.
func Test_WebSocketProxy_SetCookieForwarded(t *testing.T) {
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123", Path: "/"})
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, _ = conn.Read(r.Context())
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, resp, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.NotNil(t, resp)

	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "session" && c.Value == "abc123" {
			found = true
			break
		}
	}
	assert.True(t, found, "Set-Cookie from backend handshake must reach the client; got %v", resp.Cookies())
}

// Test_WebSocketProxy_CloseCodePreserved asserts that an application close
// code from the backend (1008 policy violation in this test) is propagated to
// the client verbatim rather than being masked as 1000. Gorilla had a subtle
// bug that occasionally rewrote codes; coder/websocket + our copyMessages
// implementation should preserve them.
func Test_WebSocketProxy_CloseCodePreserved(t *testing.T) {
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
		_ = conn.Close(websocket.StatusPolicyViolation, "policy trip")
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte("ping")))

	_, _, readErr := conn.Read(context.Background())
	require.Error(t, readErr)
	var closeErr websocket.CloseError
	require.ErrorAs(t, readErr, &closeErr)
	assert.Equal(t, websocket.StatusPolicyViolation, closeErr.Code,
		"proxy must forward the backend close code (1008) rather than substituting 1000")
	assert.Equal(t, "policy trip", closeErr.Reason,
		"proxy must forward the backend close reason unchanged")
}

func Test_WebSocketProxy_ClientCloseCodePreserved(t *testing.T) {
	type closeResult struct {
		code   websocket.StatusCode
		reason string
	}
	backendClose := make(chan closeResult, 1)
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, readErr := conn.Read(r.Context())
		var closeErr websocket.CloseError
		if errors.As(readErr, &closeErr) {
			backendClose <- closeResult{code: closeErr.Code, reason: closeErr.Reason}
			return
		}
		backendClose <- closeResult{}
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)
	h := http.Header{SandboxIngress: []string{"test-sandbox-" + strconv.Itoa(backendPort)}}
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)

	const applicationClose websocket.StatusCode = 4001
	require.NoError(t, conn.Close(applicationClose, "session expired"))

	select {
	case got := <-backendClose:
		assert.Equal(t, applicationClose, got.code)
		assert.Equal(t, "session expired", got.reason)
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not observe the client close")
	}
}

func Test_WebSocketProxy_AbruptClientDisconnectUnblocksBackend(t *testing.T) {
	backendRead := make(chan error, 1)
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, readErr := conn.Read(r.Context())
		backendRead <- readErr
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)
	h := http.Header{SandboxIngress: []string{"test-sandbox-" + strconv.Itoa(backendPort)}}
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	require.NoError(t, conn.CloseNow())

	select {
	case readErr := <-backendRead:
		require.Error(t, readErr)
	case <-time.After(5 * time.Second):
		t.Fatal("backend read remained blocked after abrupt client disconnect")
	}
}

// Test_WebSocketProxy_LargeMessage asserts that WebSocket messages above
// coder/websocket's default 32 KiB read limit still traverse the proxy. The
// gorilla-era proxy carried no such limit, and terminals or Jupyter kernels
// routinely emit larger single frames. The new bounded default must remain
// comfortably above coder/websocket's 32 KiB default.
func Test_WebSocketProxy_LargeMessage(t *testing.T) {
	const payloadSize = 128 * 1024

	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(-1)
		msgType, msg, readErr := conn.Read(r.Context())
		if readErr != nil {
			t.Errorf("backend read: %v", readErr)
			return
		}
		_ = conn.Write(r.Context(), msgType, msg)
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(-1)

	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	require.NoError(t, conn.Write(context.Background(), websocket.MessageBinary, payload))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgType, echoed, err := conn.Read(ctx)
	require.NoError(t, err, "large message must survive the proxy — 32 KiB cap regression will surface as StatusMessageTooBig")
	assert.Equal(t, websocket.MessageBinary, msgType)
	assert.Equal(t, len(payload), len(echoed))
	assert.Equal(t, payload, echoed)
}

func Test_WebSocketProxy_MessageSizeLimit(t *testing.T) {
	const limit = 1024

	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, _ = conn.Read(r.Context())
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux, WithWebSocketMessageSizeLimit(limit))
	h := http.Header{SandboxIngress: []string{"test-sandbox-" + strconv.Itoa(backendPort)}}
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	require.NoError(t, conn.Write(context.Background(), websocket.MessageBinary, make([]byte, limit+1)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	require.Error(t, readErr)
	assert.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(readErr))
}

func Test_WebSocketProxy_BackendMessageSizeLimit(t *testing.T) {
	const limit = 1024

	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = conn.Write(r.Context(), websocket.MessageBinary, make([]byte, limit+1))
		_, _, _ = conn.Read(r.Context())
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux, WithWebSocketMessageSizeLimit(limit))
	h := http.Header{SandboxIngress: []string{"test-sandbox-" + strconv.Itoa(backendPort)}}
	conn, _, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	require.Error(t, readErr)
	assert.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(readErr))
}

// Test_WebSocketProxy_BackendHandshakeRedirectNotFollowed asserts that a
// backend returning a 3xx during the WebSocket handshake is surfaced to the
// caller as-is instead of being followed. coder/websocket.Dial's default
// http.Client follows redirects; gorilla's Dialer never did. Following the
// redirect at the ingress layer would route WebSocket handshake traffic to
// a target the sandbox route did not resolve, hiding the real backend
// response from operators and potentially leaking Authorization headers to
// an unrelated endpoint.
func Test_WebSocketProxy_BackendHandshakeRedirectNotFollowed(t *testing.T) {
	var loginHits int32
	loginMux := http.NewServeMux()
	loginMux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>login page</html>"))
	})
	loginBackend := httptest.NewServer(loginMux)
	t.Cleanup(loginBackend.Close)

	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, _ *http.Request) {
		// Simulate a session-expired auth guard that would normally redirect
		// browsers to a login page. A WebSocket client cannot meaningfully
		// follow this — the redirect target is HTML, not WS.
		w.Header().Set("Location", loginBackend.URL+"/login")
		w.WriteHeader(http.StatusFound)
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	// The test client must also refuse to follow redirects — otherwise it
	// would follow the 302 that the ingress correctly forwarded, hit the
	// login endpoint from *its own* HTTP stack, and produce false-positive
	// loginHits that has nothing to do with the ingress under test.
	noFollowClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, resp, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{
		HTTPHeader: h,
		HTTPClient: noFollowClient,
	})
	require.Error(t, err)
	if conn != nil {
		_ = conn.CloseNow()
	}
	require.NotNil(t, resp, "backend redirect response must reach the client, not be silently followed")
	assert.Equal(t, http.StatusFound, resp.StatusCode,
		"the original 302 from the backend must be surfaced; a 200 here would mean the proxy followed the redirect")
	assert.Equal(t, int32(0), atomic.LoadInt32(&loginHits),
		"the redirect target must not be contacted by the ingress")
}

// Test_WebSocketProxy_BackendHandshake4xxPassthrough asserts that a backend
// rejecting the WebSocket handshake with 4xx (401 auth required, 403
// forbidden, 404 missing endpoint, ...) is surfaced to the client verbatim.
// #1117 review flagged the gorilla-era proxy for occasionally rewriting such
// responses as 502/503; handleBackendDialError uses copyResponse to preserve
// the backend response so callers can distinguish auth failures from ingress
// outages.
func Test_WebSocketProxy_BackendHandshake4xxPassthrough(t *testing.T) {
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"test\"")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing_token"}`))
	})

	proxyURL, backendPort := startProxyForBehaviorTest(t, backendMux)

	h := http.Header{}
	h.Set(SandboxIngress, "test-sandbox-"+strconv.Itoa(backendPort))
	conn, resp, err := websocket.Dial(context.Background(), proxyURL+"/ws", &websocket.DialOptions{HTTPHeader: h})
	// Dial must fail (handshake never completed), and the error carrier
	// (resp) must reflect what the backend actually sent.
	require.Error(t, err)
	if conn != nil {
		_ = conn.CloseNow()
	}
	require.NotNil(t, resp, "proxy must surface the backend handshake response so callers can diagnose auth failures")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"backend 401 must reach the client instead of being rewritten as 502/503")
	assert.Equal(t, "Bearer realm=\"test\"", resp.Header.Get("WWW-Authenticate"),
		"auth challenge header from the backend must be forwarded")
}
