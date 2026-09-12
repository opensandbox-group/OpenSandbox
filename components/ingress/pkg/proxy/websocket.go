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
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/coder/websocket"
)

const (
	// backendHandshakeTimeout bounds how long the proxy will wait for the
	// backend WebSocket handshake to complete. Matches the gorilla default so
	// that operator-facing behavior is unchanged.
	backendHandshakeTimeout = 45 * time.Second

	// defaultWebSocketMessageSizeLimit permits large terminal and Jupyter
	// messages while bounding the allocation made by websocket.Conn.Read.
	defaultWebSocketMessageSizeLimit int64 = 64 << 20
)

// WebSocketProxy reverse-proxies an HTTP/1.1 WebSocket upgrade (RFC 6455) to
// a backend WebSocket server. It does not natively accept RFC 8441 HTTP/2
// Extended CONNECT — coder/websocket v1.8.15 rejects any non-GET method in
// Accept (see coder/websocket#4). Operators terminating h2 at an L7 frontend
// must configure that frontend to translate h2 into an h1 Upgrade before it
// reaches the ingress; see docs/components/ingress.md "L7 Frontend
// Configuration for WebSocket".
type WebSocketProxy struct {
	responseObserver func(*http.Response)
	errorObserver    func(error)

	// director, if non-nil, may copy additional request headers from the
	// incoming WebSocket connection into the headers forwarded to the backend.
	director func(incoming *http.Request, out http.Header)

	// backend returns the backend URL that the proxy uses to reverse-proxy the
	// incoming WebSocket connection. The argument is the initial incoming
	// unmodified request.
	backend func(*http.Request) *url.URL

	// httpClient is used by coder/websocket.Dial to reach the backend. Callers
	// wire in an observed client (see newObservedWebSocketHTTPClient) so
	// connectivity metrics keep working across the library swap.
	httpClient *http.Client

	// messageSizeLimit bounds one complete message in either direction.
	messageSizeLimit int64
}

// NewWebSocketProxy returns a new WebSocket reverse proxy that rewrites the
// scheme, host, path, and query onto target.
func NewWebSocketProxy(target *url.URL, responseObserver func(*http.Response)) *WebSocketProxy {
	backend := func(r *http.Request) *url.URL {
		u := *target
		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawPath = r.URL.RawPath
		u.RawQuery = r.URL.RawQuery
		return &u
	}
	return &WebSocketProxy{
		backend:          backend,
		responseObserver: responseObserver,
		messageSizeLimit: defaultWebSocketMessageSizeLimit,
	}
}

// ServeHTTP dials the backend, upgrades the client, and copies WebSocket
// frames in both directions until either side closes. Only HTTP/1.1 upgrades
// reach this handler: isWebSocketRequest filters out non-GET methods, and
// coder/websocket.Accept enforces the RFC 6455 handshake shape end-to-end.
func (w *WebSocketProxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if w.backend == nil {
		http.Error(rw, "WebSocketProxy: backend is not defined", http.StatusInternalServerError)
		return
	}

	backendURL := w.backend(r)
	if backendURL == nil {
		http.Error(rw, "WebSocketProxy: backend URL is nil", http.StatusInternalServerError)
		return
	}

	clientSubprotocols := parseClientSubprotocols(r)
	requestHeader := buildBackendRequestHeader(r)
	if w.director != nil {
		w.director(r, requestHeader)
	}

	dialCtx, cancelDial := context.WithTimeout(r.Context(), backendHandshakeTimeout)
	defer cancelDial()

	// Wrap the client so the backend handshake never follows redirects.
	// coder/websocket.Dial delegates to net/http's default Client behavior,
	// which follows 3xx transparently; the gorilla-era proxy exposed the 3xx
	// to the caller instead. Following redirects during a WebSocket
	// handshake is unsafe — the redirect target rarely speaks WebSocket, and
	// the proxy would return a response from a different endpoint than the
	// one the sandbox route resolved to.
	handshakeClient := clientWithoutRedirects(w.httpClient)

	backendConn, backendResp, dialErr := websocket.Dial(dialCtx, backendURL.String(), &websocket.DialOptions{
		HTTPClient:   handshakeClient,
		HTTPHeader:   requestHeader,
		Host:         r.Host,
		Subprotocols: clientSubprotocols,
	})
	if dialErr != nil {
		w.handleBackendDialError(rw, r, backendResp, dialErr)
		return
	}
	defer func() { _ = backendConn.CloseNow() }()
	messageSizeLimit := w.messageSizeLimit
	if messageSizeLimit <= 0 {
		messageSizeLimit = defaultWebSocketMessageSizeLimit
	}
	backendConn.SetReadLimit(messageSizeLimit)

	// Forward Set-Cookie from the backend handshake response. gorilla's proxy
	// used to explicitly copy this header, and some backends (code-server for
	// example) refresh session cookies during the handshake — dropping them
	// would silently break sticky sessions.
	upgradeResponseHeaders := http.Header{}
	if backendResp != nil {
		for _, cookie := range backendResp.Header.Values(SetCookie) {
			upgradeResponseHeaders.Add(SetCookie, cookie)
		}
	}
	for k, vs := range upgradeResponseHeaders {
		rw.Header()[k] = vs
	}

	clientConn, acceptErr := websocket.Accept(rw, r, &websocket.AcceptOptions{
		Subprotocols: []string{backendConn.Subprotocol()},
		// The ingress always sits behind trusted gateways where Host and
		// Origin diverge (browser UI vs internal target). Same-origin
		// rejection is enforced upstream; here we accept any Origin, matching
		// the old gorilla behavior (CheckOrigin returned true).
		InsecureSkipVerify: true,
	})
	if acceptErr != nil {
		Logger.With(slogger.Field{Key: "error", Value: acceptErr}).Errorf("WebSocketProxy: couldn't upgrade client connection")
		return
	}
	defer func() { _ = clientConn.CloseNow() }()
	clientConn.SetReadLimit(messageSizeLimit)

	relayFrames(r.Context(), clientConn, backendConn)
}

// handleBackendDialError surfaces the failure to the client while preserving
// as much of the backend response as possible. When the backend rejected the
// handshake with a real HTTP status (401 auth required, 403 forbidden, 404
// missing endpoint, ...), we replay that response so callers can distinguish
// application failures from ingress outages. When there was no response at
// all (TCP-level failure), we fall back to 503 Service Unavailable and let
// the connectivity observer record the miss.
func (w *WebSocketProxy) handleBackendDialError(rw http.ResponseWriter, r *http.Request, backendResp *http.Response, dialErr error) {
	if backendResp != nil && w.responseObserver != nil {
		w.responseObserver(backendResp)
	}
	if backendResp == nil && r.Context().Err() == nil && w.errorObserver != nil {
		w.errorObserver(dialErr)
	}
	Logger.With(slogger.Field{Key: "error", Value: dialErr}).Errorf("WebSocketProxy: couldn't dial to remote backend")
	if backendResp != nil {
		if copyErr := copyResponse(rw, backendResp); copyErr != nil {
			Logger.With(slogger.Field{Key: "error", Value: copyErr}).Errorf("WebSocketProxy: couldn't relay backend handshake response")
		}
		return
	}
	http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

// parseClientSubprotocols extracts the WebSocket subprotocols the client
// offered so we can propagate them to the backend Dial and echo the negotiated
// one back to the client on Accept. Sec-WebSocket-Protocol values are
// comma-separated tokens (RFC 6455 §4.2.2).
func parseClientSubprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values(SecWebSocketProtocol) {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

// buildBackendRequestHeader copies request headers from the client, stripping
// hop-by-hop headers (RFC 7230 §6.1), any header named by a Connection token,
// h2 pseudo-headers, and WebSocket handshake headers that coder/websocket
// manages itself. It then appends the forwarding headers required by upstream
// backends (X-Forwarded-For, X-Forwarded-Proto).
func buildBackendRequestHeader(r *http.Request) http.Header {
	// Collect the union of Connection tokens so headers named by them are
	// stripped alongside the fixed hop-by-hop list.
	connTokens := map[string]bool{}
	for _, v := range r.Header.Values(HopByHopConnection) {
		for _, token := range strings.Split(v, ",") {
			if h := http.CanonicalHeaderKey(strings.TrimSpace(token)); h != "" {
				connTokens[h] = true
			}
		}
	}

	requestHeader := http.Header{}
	for key, values := range r.Header {
		switch key {
		case HopByHopConnection, HopByHopKeepAlive, HopByHopProxyAuth, HopByHopProxyAuthz,
			HopByHopTE, HopByHopTrailer, HopByHopTransferEncoding, HopByHopUpgrade,
			HopByHopProxyConnection,
			SecWebSocketKey, SecWebSocketVersion, SecWebSocketExtensions, SecWebSocketProtocol:
			continue
		}
		if connTokens[key] {
			continue
		}
		// Defensive: drop any h2 pseudo-header a caller managed to place in
		// r.Header. This ingress does not natively accept RFC 8441 h2
		// requests (isWebSocketRequest filters them out before Dial is
		// called), so under normal operation nothing here begins with ":".
		// This branch guards against future refactors accidentally letting
		// a pseudo-header through, which would trigger "invalid header
		// field name" rejections on the HTTP/1.1 backend leg.
		if strings.HasPrefix(key, ":") {
			continue
		}
		for _, v := range values {
			requestHeader.Add(key, v)
		}
	}

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior, ok := r.Header[XForwardedFor]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set(XForwardedFor, clientIP)
	}

	requestHeader.Set(XForwardedProto, "http")
	if r.TLS != nil {
		requestHeader.Set(XForwardedProto, "https")
	}
	return requestHeader
}

// relayFrames copies WebSocket messages in both directions between client and
// backend until either side closes or errors. Close codes and reasons are
// preserved so applications relying on codes like 4001 or 1008 see the peer's
// intent instead of a generic 1000. The ingress does not interpret payloads.
func relayFrames(ctx context.Context, client, backend *websocket.Conn) {
	// Run each direction in its own goroutine. The first to return signals the
	// other to stop by closing its source conn (CloseNow is idempotent and
	// unblocks the other pump's Read).
	errCh := make(chan error, 2)
	go func() { errCh <- copyMessages(ctx, client, backend) }()
	go func() { errCh <- copyMessages(ctx, backend, client) }()

	firstErr := <-errCh
	// Unblock the second pump by aborting both conns; CloseNow is safe to call
	// concurrently with an in-flight Read.
	_ = client.CloseNow()
	_ = backend.CloseNow()
	<-errCh

	if firstErr != nil && !isBenignCloseError(firstErr) {
		Logger.With(slogger.Field{Key: "error", Value: firstErr}).Warnf("WebSocketProxy: relay ended with error")
	}
}

// copyMessages reads messages from src and writes them to dst until either
// side errors. It returns the terminating error, if any.
func copyMessages(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		msgType, data, readErr := src.Read(ctx)
		if readErr != nil {
			// Forward the peer's close code/reason so the other side observes
			// the same protocol-level status. If the read failed for reasons
			// other than a graceful close (network reset, timeout, context
			// cancellation) we let the CloseNow in relayFrames tear the
			// connection down without inventing a status code.
			if closeErr := new(websocket.CloseError); errors.As(readErr, closeErr) {
				_ = dst.Close(closeErr.Code, closeErr.Reason)
			} else if errors.Is(readErr, websocket.ErrMessageTooBig) {
				_ = dst.Close(websocket.StatusMessageTooBig, "message exceeds proxy limit")
			}
			return readErr
		}
		if writeErr := dst.Write(ctx, msgType, data); writeErr != nil {
			return writeErr
		}
	}
}

// isBenignCloseError reports whether err represents a normal termination that
// should not be logged as a warning.
func isBenignCloseError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}

// clientWithoutRedirects returns an *http.Client that shares base's Transport
// and settings but refuses to follow 3xx responses during the WebSocket
// handshake. base may be nil, in which case a fresh Client is returned. The
// returned Client is safe to hand to a single coder/websocket.Dial call —
// coder/websocket may further mutate CheckRedirect (to fix ws→http scheme
// rewriting on redirect targets it never actually follows), so callers should
// not share this Client with unrelated HTTP traffic.
func clientWithoutRedirects(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.Body == nil {
		return nil
	}
	_, err := io.Copy(rw, resp.Body)
	return err
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
