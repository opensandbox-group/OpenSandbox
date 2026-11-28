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
	backendHandshakeTimeout = 45 * time.Second

	// Bound full-message allocations while allowing terminal and Jupyter payloads.
	defaultWebSocketMessageSizeLimit int64 = 64 << 20
)

type webSocketProxy struct {
	responseObserver func(*http.Response)
	errorObserver    func(error)
	outcomeObserver  func(string)

	director func(incoming *http.Request, out http.Header)

	backend func(*http.Request) *url.URL

	httpClient *http.Client

	// messageSizeLimit bounds one complete message in either direction.
	messageSizeLimit int64
}

func newWebSocketProxy(target *url.URL, responseObserver func(*http.Response)) *webSocketProxy {
	backend := func(r *http.Request) *url.URL {
		u := *target
		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawPath = r.URL.RawPath
		u.RawQuery = r.URL.RawQuery
		return &u
	}
	return &webSocketProxy{
		backend:          backend,
		responseObserver: responseObserver,
		messageSizeLimit: defaultWebSocketMessageSizeLimit,
	}
}

func (w *webSocketProxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
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

	// Redirects must not send a handshake to an endpoint outside the resolved route.
	handshakeClient := clientWithoutRedirects(w.httpClient)

	// coder/websocket owns the response body; Dial documents that callers must not close it.
	backendConn, backendResp, dialErr := websocket.Dial(dialCtx, backendURL.String(), &websocket.DialOptions{ //nolint:bodyclose
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

	// Backends may refresh session cookies during the handshake.
	upgradeResponseHeaders := http.Header{}
	if backendResp != nil {
		for _, cookie := range backendResp.Header.Values(setCookie) {
			upgradeResponseHeaders.Add(setCookie, cookie)
		}
	}
	for k, vs := range upgradeResponseHeaders {
		rw.Header()[k] = vs
	}

	clientConn, acceptErr := websocket.Accept(rw, r, &websocket.AcceptOptions{
		Subprotocols: []string{backendConn.Subprotocol()},
		// Host and Origin may differ; the fronting gateway must enforce origin policy.
		InsecureSkipVerify: true,
	})
	if acceptErr != nil {
		Logger.With(slogger.Field{Key: "error", Value: acceptErr}).Errorf("WebSocketProxy: couldn't upgrade client connection")
		return
	}
	defer func() { _ = clientConn.CloseNow() }()
	clientConn.SetReadLimit(messageSizeLimit)

	err := relayFrames(r.Context(), clientConn, backendConn)
	if w.outcomeObserver != nil {
		outcome := "upstream_response"
		if err != nil {
			outcome = "stream_aborted"
		}
		w.outcomeObserver(outcome)
	}
}

// Preserve backend handshake rejections; connection failures without a response are 503.
func (w *webSocketProxy) handleBackendDialError(rw http.ResponseWriter, r *http.Request, backendResp *http.Response, dialErr error) {
	if backendResp != nil && w.responseObserver != nil {
		w.responseObserver(backendResp)
	}
	if backendResp == nil && r.Context().Err() == nil && w.errorObserver != nil {
		if config := fastPathDialConfigFromContext(r.Context()); config == nil || !config.alreadyNotified() {
			w.errorObserver(dialErr)
		}
	}
	Logger.With(slogger.Field{Key: "error", Value: dialErr}).Errorf("WebSocketProxy: couldn't dial to remote backend")
	if backendResp != nil {
		if copyErr := copyResponse(rw, backendResp); copyErr != nil {
			Logger.With(slogger.Field{Key: "error", Value: copyErr}).Errorf("WebSocketProxy: couldn't relay backend handshake response")
			if w.outcomeObserver != nil {
				w.outcomeObserver("stream_aborted")
			}
		}
		return
	}
	http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func parseClientSubprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values(secWebSocketProtocol) {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

func buildBackendRequestHeader(r *http.Request) http.Header {
	// Connection can name additional hop-by-hop headers.
	connTokens := map[string]bool{}
	for _, v := range r.Header.Values(hopByHopConnection) {
		for _, token := range strings.Split(v, ",") {
			if h := http.CanonicalHeaderKey(strings.TrimSpace(token)); h != "" {
				connTokens[h] = true
			}
		}
	}

	requestHeader := http.Header{}
	for key, values := range r.Header {
		switch key {
		case hopByHopConnection, hopByHopKeepAlive, hopByHopProxyAuth, hopByHopProxyAuthz,
			hopByHopTE, hopByHopTrailer, hopByHopTransferEncoding, hopByHopUpgrade,
			hopByHopProxyConnection,
			secWebSocketKey, secWebSocketVersion, secWebSocketExtensions, secWebSocketProtocol:
			continue
		}
		if connTokens[key] {
			continue
		}
		// HTTP/2 pseudo-headers are invalid on the HTTP/1.1 backend leg.
		if strings.HasPrefix(key, ":") {
			continue
		}
		for _, v := range values {
			requestHeader.Add(key, v)
		}
	}

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior, ok := r.Header[xForwardedFor]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set(xForwardedFor, clientIP)
	}

	requestHeader.Set(xForwardedProto, "http")
	if r.TLS != nil {
		requestHeader.Set(xForwardedProto, "https")
	}
	return requestHeader
}

func relayFrames(ctx context.Context, client, backend *websocket.Conn) error {
	errCh := make(chan error, 2)
	go func() { errCh <- copyMessages(ctx, client, backend) }()
	go func() { errCh <- copyMessages(ctx, backend, client) }()

	firstErr := <-errCh
	// Closing both connections unblocks the remaining pump's Read.
	_ = client.CloseNow()
	_ = backend.CloseNow()
	<-errCh

	if firstErr != nil && !isBenignCloseError(firstErr) {
		Logger.With(slogger.Field{Key: "error", Value: firstErr}).Warnf("WebSocketProxy: relay ended with error")
		return firstErr
	}
	return nil
}

func copyMessages(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		msgType, data, readErr := src.Read(ctx)
		if readErr != nil {
			// Preserve peer close codes; transport failures must not invent a graceful close.
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

// Dial may mutate CheckRedirect; clone the client but share its transport.
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
	// The marker is gateway-owned; upstream responses must not carry it.
	resp.Header.Del(WakeMarkerHeader)
	// coder/websocket replaces a failed handshake body with its first 1 KiB,
	// so the backend's Content-Length no longer describes it.
	resp.Header.Del("Content-Length")
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
