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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/gorilla/websocket"
)

var (
	// defaultWebSocketDialer is a dialer with all fields set to the default zero values.
	defaultWebSocketDialer = websocket.DefaultDialer

	// defaultUpgrader specifies the parameters for upgrading an HTTP
	// connection to a WebSocket connection.
	defaultUpgrader = &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// Allow any Origin: ingress sits behind trusted gateways where Host/Origin
		// often diverge (e.g. browser UI vs internal target). gorilla's default
		// same-origin check rejects those upgrades.
		CheckOrigin: func(_ *http.Request) bool { return true },
	}
)

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

	//  dialer contains options for connecting to the backend WebSocket server.
	//  If nil, DefaultDialer is used.
	dialer *websocket.Dialer

	// upgrader specifies the parameters for upgrading a incoming HTTP
	// connection to a WebSocket connection. If nil, DefaultUpgrader is used.
	upgrader *websocket.Upgrader
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

	dialer := w.dialer
	if w.dialer == nil {
		dialer = defaultWebSocketDialer
	}

	// Pass headers from the incoming request to the dialer to forward them to
	// the final destinations.
	requestHeader := http.Header{}
	if origin := r.Header.Get(Origin); origin != "" {
		requestHeader.Add(Origin, origin)
	}
	for _, prot := range r.Header[SecWebSocketProtocol] {
		requestHeader.Add(SecWebSocketProtocol, prot)
	}
	for _, cokiee := range r.Header[Cookie] {
		requestHeader.Add(Cookie, cokiee)
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

	// Connect to the backend URL, also pass the headers we get from the requst
	// together with the Forwarded headers we prepared above.
	connBackend, resp, err := dialer.Dial(backendURL.String(), requestHeader)
	if err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't dial to remote backend")
		if resp != nil {
			// If the WebSocket handshake fails, ErrBadHandshake is returned
			// along with a non-nil *http.Response so that callers can handle
			// redirects, authentication, etcetera.
			if err := copyResponse(rw, resp); err != nil {
				Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't write response after failed remote backend handshake")
			}
		} else {
			http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	defer connBackend.Close()

	upgrader := w.upgrader
	if w.upgrader == nil {
		upgrader = defaultUpgrader
	}

	// Only pass those headers to the upgrader.
	upgradeHeader := http.Header{}
	if hdr := resp.Header.Get(SecWebSocketProtocol); hdr != "" {
		upgradeHeader.Set(SecWebSocketProtocol, hdr)
	}
	if hdr := resp.Header.Get(SetCookie); hdr != "" {
		upgradeHeader.Set(SetCookie, hdr)
	}

	// Now upgrade the existing incoming request to a WebSocket connection.
	// Also pass the header that we gathered from the Dial handshake.
	connPub, err := upgrader.Upgrade(rw, r, upgradeHeader)
	if err != nil {
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf("WebSocketProxy: couldn't upgrade websocket connection")
		return
	}
	defer connPub.Close()

	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)
	replicateWebsocketConn := func(dst, src *websocket.Conn, errc chan error) {
		for {
			msgType, msg, err := src.ReadMessage()
			if err != nil {
				m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
				if e, ok := err.(*websocket.CloseError); ok { //nolint:errorlint
					if e.Code != websocket.CloseNoStatusReceived {
						m = websocket.FormatCloseMessage(e.Code, e.Text)
					}
				}
				errc <- err
				_ = dst.WriteMessage(websocket.CloseMessage, m)
				break
			}
			err = dst.WriteMessage(msgType, msg)
			if err != nil {
				errc <- err
				break
			}
		}
	}

	go replicateWebsocketConn(connPub, connBackend, errClient)
	go replicateWebsocketConn(connBackend, connPub, errBackend)

	var message string
	select {
	case err = <-errClient:
		message = "WebSocketProxy: Error when copying from backend to client: %v"
	case err = <-errBackend:
		message = "WebSocketProxy: Error when copying from client to backend: %v"

	}
	if e, ok := err.(*websocket.CloseError); !ok || e.Code == websocket.CloseAbnormalClosure { //nolint:errorlint
		Logger.With(slogger.Field{Key: "error", Value: err}).Errorf(message, err)
	}
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
