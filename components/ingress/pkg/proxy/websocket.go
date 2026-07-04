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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/coder/websocket"
)

const backendHandshakeTimeout = 45 * time.Second

type handshakeError struct {
	statusCode int
	statusLine string
}

func (e *handshakeError) Error() string { return e.statusLine }

// WebSocketProxy is an HTTP Handler that takes an incoming WebSocket
// connection and proxies it to another server.
type WebSocketProxy struct {
	// director, if non-nil, is a function that may copy additional request
	// headers from the incoming WebSocket connection into the output headers
	// which will be forwarded to another server.
	director func(incoming *http.Request, out http.Header)

	// backend returns the backend URL which the proxy uses to reverse proxy
	// the incoming WebSocket connection. Request is the initial incoming and
	// unmodified request.
	backend func(*http.Request) *url.URL
}

// ProxyHandler returns a new http.Handler interface that reverse proxies the
// request to the given target.
func ProxyHandler(target *url.URL) http.Handler { return NewWebSocketProxy(target) }

// NewWebSocketProxy returns a new Websocket reverse proxy that rewrites the
// URL's to the scheme, host and base path provider in target.
func NewWebSocketProxy(target *url.URL) *WebSocketProxy {
	backend := func(r *http.Request) *url.URL {
		// Shallow copy
		u := *target
		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return &u
	}
	return &WebSocketProxy{backend: backend}
}

//nolint:gocognit
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

	// Forward all incoming headers to the backend except hop-by-hop headers
	// (RFC 7230 §6.1) and WebSocket handshake headers managed by the dialer.
	// Per RFC 7230, also strip any header named by Connection tokens.
	connTokens := map[string]bool{}
	for _, v := range r.Header[HopByHopConnection] {
		for _, token := range strings.Split(v, ",") {
			if h := http.CanonicalHeaderKey(strings.TrimSpace(token)); h != "" {
				connTokens[h] = true
			}
		}
	}

	// Collect client-requested subprotocols before filtering headers.
	var clientSubprotocols []string
	for _, v := range r.Header[SecWebSocketProtocol] {
		for _, sp := range strings.Split(v, ",") {
			if sp = strings.TrimSpace(sp); sp != "" {
				clientSubprotocols = append(clientSubprotocols, sp)
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
		// Strip h2 pseudo-headers — invalid in h1 backend requests.
		if strings.HasPrefix(key, ":") {
			continue
		}
		for _, v := range values {
			requestHeader.Add(key, v)
		}
	}
	if r.Host != "" {
		requestHeader.Set(Host, r.Host)
	}

	// Pass X-Forwarded-For headers too, code below is a part of
	// httputil.ReverseProxy. See http://en.wikipedia.org/wiki/X-Forwarded-For
	// for more information
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := r.Header[XForwardedFor]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set(XForwardedFor, clientIP)
	}

	// Set the originating protocol of the incoming HTTP request. The SSL might
	// be terminated on our site and because we doing proxy adding this would
	// be helpful for applications on the backend.
	requestHeader.Set(XForwardedProto, "http")
	if r.TLS != nil {
		requestHeader.Set(XForwardedProto, "https")
	}

	// Enable the director to copy any additional headers it desires for
	// forwarding to the remote server.
	if w.director != nil {
		w.director(r, requestHeader)
	}

	// HTTP/2 Extended CONNECT (RFC 8441) — raw bidirectional tunnel.
	if r.ProtoMajor >= 2 && r.Method == http.MethodConnect {
		w.serveH2Tunnel(rw, r, backendURL, requestHeader, clientSubprotocols)
		return
	}

	w.serveH1(rw, r, backendURL, requestHeader, clientSubprotocols)
}

// serveH1 handles the traditional HTTP/1.1 WebSocket upgrade path.
func (w *WebSocketProxy) serveH1(rw http.ResponseWriter, r *http.Request, backendURL *url.URL, requestHeader http.Header, clientSubprotocols []string) {
	ctx := r.Context()
	dialCtx, dialCancel := context.WithTimeout(ctx, backendHandshakeTimeout)
	defer dialCancel()

	// Dial the backend first so we can relay errors before upgrading the client.
	connBackend, resp, err := websocket.Dial(dialCtx, backendURL.String(), &websocket.DialOptions{
		HTTPHeader:   requestHeader,
		Subprotocols: clientSubprotocols,
		Host:         requestHeader.Get("Host"),
	})
	if err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't dial to remote backend")
		if resp != nil {
			if copyErr := copyResponse(rw, resp); copyErr != nil {
				Logger.With(slogger.Field{Key: "error", Value: copyErr}).Errorf("WebSocketProxy: couldn't write response after failed remote backend handshake")
			}
		} else {
			http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	defer connBackend.CloseNow() //nolint:errcheck
	connBackend.SetReadLimit(-1)

	// Copy Set-Cookie from the backend handshake response before upgrading.
	if resp != nil {
		for _, c := range resp.Header.Values(SetCookie) {
			rw.Header().Add(SetCookie, c)
		}
	}

	// Accept the client-side WebSocket upgrade.
	connPub, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		Subprotocols:       subprotocolsFromResponse(resp),
	})
	if err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't upgrade websocket connection")
		return
	}
	defer connPub.CloseNow() //nolint:errcheck
	connPub.SetReadLimit(-1)

	// Bidirectional relay.
	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)

	go replicateConn(ctx, connPub, connBackend, errClient)
	go replicateConn(ctx, connBackend, connPub, errBackend)

	var message string
	select {
	case err = <-errClient:
		message = "WebSocketProxy: Error when copying from backend to client: %v"
	case err = <-errBackend:
		message = "WebSocketProxy: Error when copying from client to backend: %v"
	}

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code == websocket.StatusAbnormalClosure {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf(message, err)
	}
}

// serveH2Tunnel handles HTTP/2 Extended CONNECT (RFC 8441) by dialing
// the backend over raw HTTP/1.1 and tunneling bytes between the h2 stream
// and the backend TCP connection. Both sides carry WebSocket frame bytes,
// so no re-framing is needed.
func (w *WebSocketProxy) serveH2Tunnel(rw http.ResponseWriter, r *http.Request, backendURL *url.URL, requestHeader http.Header, clientSubprotocols []string) {
	backendAddr := backendURL.Host
	if !strings.Contains(backendAddr, ":") {
		backendAddr += ":80"
	}

	backendConn, err := net.DialTimeout("tcp", backendAddr, backendHandshakeTimeout)
	if err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't connect to remote backend (h2 tunnel)")
		http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer backendConn.Close()

	// Perform the WebSocket handshake over the raw connection.
	respHeaders, err := rawWebSocketHandshake(backendConn, backendURL, requestHeader, clientSubprotocols)
	if err != nil {
		var hsErr *handshakeError
		if errors.As(err, &hsErr) {
			http.Error(rw, hsErr.statusLine, hsErr.statusCode)
		} else {
			Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: backend WebSocket handshake failed (h2 tunnel)")
			http.Error(rw, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		}
		return
	}

	rc := http.NewResponseController(rw)
	if err := rc.EnableFullDuplex(); err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: EnableFullDuplex failed")
		http.Error(rw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	// Forward Set-Cookie from backend handshake before writing the h2 200.
	for _, c := range respHeaders.Values("Set-Cookie") {
		rw.Header().Add("Set-Cookie", c)
	}
	rw.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: flush failed")
		return
	}

	// Both sides now carry raw WebSocket frame bytes — copy bidirectionally.
	flusher, _ := rw.(http.Flusher)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(backendConn, r.Body)
		// Half-close the write side so backend can finish sending.
		if tc, ok := backendConn.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		} else {
			backendConn.Close()
		}
	}()
	copyFlush(rw, backendConn, flusher)
	r.Body.Close()
	<-done
}

// rawWebSocketHandshake sends an HTTP/1.1 WebSocket upgrade request on conn
// and verifies the 101 response. Returns parsed response headers on success.
func rawWebSocketHandshake(conn net.Conn, target *url.URL, extraHeaders http.Header, subprotocols []string) (http.Header, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate Sec-WebSocket-Key: %w", err)
	}
	secKey := base64.StdEncoding.EncodeToString(key)

	path := target.RequestURI()
	if path == "" {
		path = "/"
	}

	var buf strings.Builder
	buf.WriteString("GET " + path + " HTTP/1.1\r\n")
	buf.WriteString("Host: " + target.Host + "\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Version: 13\r\n")
	buf.WriteString("Sec-WebSocket-Key: " + secKey + "\r\n")
	if len(subprotocols) > 0 {
		buf.WriteString("Sec-WebSocket-Protocol: " + strings.Join(subprotocols, ", ") + "\r\n")
	}
	for k, vs := range extraHeaders {
		if k == "Host" {
			continue
		}
		for _, v := range vs {
			buf.WriteString(k + ": " + v + "\r\n")
		}
	}
	buf.WriteString("\r\n")

	if err := conn.SetDeadline(time.Now().Add(backendHandshakeTimeout)); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, buf.String()); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	// Read the full HTTP response up to the end-of-headers marker (\r\n\r\n).
	const maxHandshakeResponseSize = 16384
	var resp []byte
	single := make([]byte, 1)
	for {
		if _, err := conn.Read(single); err != nil {
			return nil, fmt.Errorf("read handshake response: %w", err)
		}
		resp = append(resp, single[0])
		if len(resp) >= 4 && string(resp[len(resp)-4:]) == "\r\n\r\n" {
			break
		}
		if len(resp) > maxHandshakeResponseSize {
			return nil, fmt.Errorf("handshake response too large")
		}
	}

	end := bytes.IndexByte(resp, '\r')
	if end < 0 {
		end = len(resp)
	}
	statusLine := string(resp[:end])
	if !strings.Contains(statusLine, "101") {
		code := parseHTTPStatusCode(statusLine)
		return nil, &handshakeError{statusCode: code, statusLine: statusLine}
	}

	// Parse response headers.
	respHeaders := http.Header{}
	headerLines := strings.Split(string(resp), "\r\n")
	for i := 1; i < len(headerLines); i++ {
		line := headerLines[i]
		if line == "" {
			break
		}
		colonIdx := strings.IndexByte(line, ':')
		if colonIdx < 0 {
			continue
		}
		respHeaders.Add(
			strings.TrimSpace(line[:colonIdx]),
			strings.TrimSpace(line[colonIdx+1:]),
		)
	}

	if !strings.EqualFold(respHeaders.Get("Upgrade"), "websocket") ||
		!headerContainsToken(respHeaders, "Connection", "Upgrade") {
		return nil, fmt.Errorf("invalid WebSocket handshake: missing Upgrade/Connection headers")
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return respHeaders, nil
}

func parseHTTPStatusCode(statusLine string) int {
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) >= 2 {
		if code, err := strconv.Atoi(parts[1]); err == nil {
			return code
		}
	}
	return http.StatusBadGateway
}

func replicateConn(ctx context.Context, dst, src *websocket.Conn, errc chan error) {
	for {
		msgType, msg, err := src.Read(ctx)
		if err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				dst.Close(closeErr.Code, closeErr.Reason)
			} else {
				dst.Close(websocket.StatusNormalClosure, "")
			}
			errc <- err
			break
		}
		err = dst.Write(ctx, msgType, msg)
		if err != nil {
			errc <- err
			break
		}
	}
}

func subprotocolsFromResponse(resp *http.Response) []string {
	if resp == nil {
		return nil
	}
	if proto := resp.Header.Get(SecWebSocketProtocol); proto != "" {
		return []string{proto}
	}
	return nil
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()

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

// copyFlush copies from src to dst, flushing after each read chunk so that
// small WebSocket frames reach the h2 client without waiting for more data.
func copyFlush(dst io.Writer, src io.Reader, flusher http.Flusher) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}
