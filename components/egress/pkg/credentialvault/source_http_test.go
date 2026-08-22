// Copyright 2026 Alibaba Group Holding Ltd.
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

package credentialvault

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testProviderHost is a non-loopback hostname used in test URLs so that
// httpSourceFactory does not reject them at write time. The dialer installed
// by newRedirectingSource actually connects to the httptest.Server's loopback
// address, so no real DNS lookup happens.
const testProviderHost = "vault.test"

// redirectingDialer returns a DialContext that ignores its address argument
// and always dials the given httptest.Server's listener address instead. This
// lets tests use a non-loopback URL (which passes validateHTTPSourceURL) while
// still exercising the real HTTP client against an in-process server.
func redirectingDialer(srv *httptest.Server) func(ctx context.Context, network, addr string) (net.Conn, error) {
	target := srv.Listener.Addr().String()
	var d net.Dialer
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, target)
	}
}

// installTestDialer replaces the transport on an httpSource so that any host
// in the URL is dialed to srv. Panics if src is not the internal *httpSource
// type or its transport is not an *http.Transport.
func installTestDialer(src CredentialSource, srv *httptest.Server) {
	hs, ok := src.(*httpSource)
	if !ok {
		panic(fmt.Sprintf("expected *httpSource, got %T", src))
	}
	tr, ok := hs.client.Transport.(*http.Transport)
	if !ok {
		panic(fmt.Sprintf("expected *http.Transport, got %T", hs.client.Transport))
	}
	tr.DialContext = redirectingDialer(srv)
}

// testProviderURL returns a URL rooted at testProviderHost with the given
// path. Callers should pair it with installTestDialer so the request reaches
// srv despite the non-loopback host.
func testProviderURL(path string) string {
	return "http://" + testProviderHost + path
}

func TestHttpSourceResolveBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":"my-secret"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)
	require.Equal(t, "http", src.Type())

	val, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "my-secret", val)
}

func TestHttpSourceCachesWithNilTTL(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"value":"cached-forever"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	for i := 0; i < 5; i++ {
		val, err := src.Resolve(context.Background())
		require.NoError(t, err)
		require.Equal(t, "cached-forever", val)
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestHttpSourceRefetchesAfterTTLExpires(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, `{"value":"v%d","ttl":0}`, n)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	v1, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v1", v1)

	v2, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v2", v2)

	require.Equal(t, int32(2), calls.Load())
}

func TestHttpSourceDynamicURLAndHeaders(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			require.Equal(t, "/initial", r.URL.Path)
			require.Equal(t, "boot", r.Header.Get("X-Auth"))
			resp := httpSourceResponse{
				Value:   "first",
				URL:     fmt.Sprintf("http://%s/refreshed", r.Host),
				Headers: map[string]string{"X-Auth": "renewed"},
				TTL:     intPtr(0),
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		require.Equal(t, "/refreshed", r.URL.Path)
		require.Equal(t, "renewed", r.Header.Get("X-Auth"))
		fmt.Fprintf(w, `{"value":"second","ttl":0}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     testProviderURL("/initial"),
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	v1, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "first", v1)

	v2, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", v2)
}

func TestHttpSourceReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "status 403")
}

func TestHttpSourceReturnsErrorOnEmptyValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":""}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "response value is empty")
}

func TestHttpSourceFactoryRejectsEmptyURL(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "",
	}))
	require.ErrorContains(t, err, "url cannot be empty")
}

func TestHttpSourceFactoryRejectsUnknownFields(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type":    "http",
		"url":     "https://example.com",
		"unknown": "field",
	}))
	require.Error(t, err)
}

func TestHttpSourceFactoryDefaultsMethodToGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		fmt.Fprintf(w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceCustomMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		fmt.Fprintf(w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":   "http",
		"url":    testProviderURL("/cred"),
		"method": "POST",
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceRegisteredInDefaultRegistry(t *testing.T) {
	r := NewSourceRegistry()
	types := r.SupportedTypes()
	found := false
	for _, typ := range types {
		if typ == "http" {
			found = true
			break
		}
	}
	require.True(t, found, "http source type should be registered in default registry")
}

func TestHttpSourceConcurrentResolveSingleFetch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"value":"shared"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			val, err := src.Resolve(context.Background())
			require.NoError(t, err)
			require.Equal(t, "shared", val)
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), calls.Load(), "singleflight should coalesce concurrent fetches into one")
}

func TestHttpSourceFactoryRejectsNonHTTPScheme(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "ftp://vault.example.com/cred",
	}))
	require.ErrorContains(t, err, "must use http or https scheme")
}

func TestHttpSourceFactoryRejectsRelativeURL(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "/token",
	}))
	require.ErrorContains(t, err, "must use http or https scheme")
}

func TestHttpSourceFactoryRejectsHostlessURL(t *testing.T) {
	for _, u := range []string{"http:/token", "https:///token", "http://"} {
		_, err := httpSourceFactory(mustMarshal(map[string]string{
			"type": "http",
			"url":  u,
		}))
		require.ErrorContains(t, err, "must include a host", u)
	}
}

func TestHttpSourceFactoryRejectsInvalidMethod(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":   "http",
		"url":    "https://vault.example.com/cred",
		"method": "BAD METHOD",
	}))
	require.ErrorContains(t, err, "invalid method")
}

func TestHttpSourceFactoryRejectsInvalidHeaderName(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     "https://vault.example.com/cred",
		"headers": map[string]string{"Bad Header": "value"},
	}))
	require.ErrorContains(t, err, "invalid header name")
}

func TestHttpSourceFactoryRejectsHeaderValueWithCRLF(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     "https://vault.example.com/cred",
		"headers": map[string]string{"X-Auth": "value\r\ninjection"},
	}))
	require.ErrorContains(t, err, "invalid character")
}

func TestHttpSourceFactoryRejectsHeaderValueWithControlChar(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     "https://vault.example.com/cred",
		"headers": map[string]string{"X-Auth": "value\x00null"},
	}))
	require.ErrorContains(t, err, "invalid character")
}

func TestHttpSourceURLOnlyRotationClearsBootstrapHeaders(t *testing.T) {
	var calls atomic.Int32
	initialURL := testProviderURL("/initial")
	rotatedURL := "http://" + testProviderHost + "/refreshed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			require.Equal(t, "boot", r.Header.Get("X-Auth"))
			fmt.Fprintf(w, `{"value":"first","url":%q,"ttl":0}`, rotatedURL)
			return
		}
		require.Equal(t, "", r.Header.Get("X-Auth"), "bootstrap header should not leak to rotated URL")
		fmt.Fprintf(w, `{"value":"second","ttl":0}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     initialURL,
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceRejectsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com", http.StatusFound)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  testProviderURL("/cred"),
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "redirects are not allowed")
}

func TestHttpSourceHeadersRotationClearsBootstrapHeaders(t *testing.T) {
	var calls atomic.Int32
	initialURL := testProviderURL("/initial")
	rotatedURL := "http://" + testProviderHost + "/refreshed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			require.Equal(t, "boot", r.Header.Get("X-Auth"))
			fmt.Fprintf(w, `{"value":"first","url":%q,"headers":{},"ttl":0}`, rotatedURL)
			return
		}
		require.Equal(t, "", r.Header.Get("X-Auth"), "bootstrap header should not leak after rotation")
		fmt.Fprintf(w, `{"value":"second","ttl":0}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     initialURL,
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceEchoedURLKeepsBootstrapHeaders(t *testing.T) {
	var calls atomic.Int32
	sameURL := testProviderURL("/cred")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		// Bootstrap headers must reach the provider on every call, since the
		// response only echoes the same refresh URL without rotating headers.
		require.Equal(t, "boot", r.Header.Get("X-Auth"),
			"bootstrap header should be preserved when URL is unchanged (call %d)", n)
		fmt.Fprintf(w, `{"value":"v%d","url":%q,"ttl":0}`, n, sameURL)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     sameURL,
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	require.Equal(t, int32(2), calls.Load())
}

func TestHttpSourceFactoryRejectsOutOfRangePort(t *testing.T) {
	cases := []string{
		"http://vault.example.com:0/cred",
		"http://vault.example.com:65536/cred",
		"http://vault.example.com:99999/cred",
	}
	for _, raw := range cases {
		_, err := httpSourceFactory(mustMarshal(map[string]string{
			"type": "http",
			"url":  raw,
		}))
		require.ErrorContains(t, err, "invalid port", "url %s should be rejected", raw)
	}
}

func TestHttpSourceFactoryRejectsLoopbackHost(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/cred",
		"http://127.5.5.5/cred",
		"http://[::1]/cred",
		"http://localhost/cred",
		"http://LocalHost:8080/cred",
	}
	for _, raw := range cases {
		_, err := httpSourceFactory(mustMarshal(map[string]string{
			"type": "http",
			"url":  raw,
		}))
		require.ErrorContains(t, err, "loopback", "url %s should be rejected", raw)
	}
}

func TestHttpSourceEquivalentEchoedURLKeepsBootstrapHeaders(t *testing.T) {
	// Provider echoes its own refresh URL in a slightly different textual
	// form on every fetch: switches host casing and adds the explicit
	// default port. The rotation logic should treat this as the same
	// endpoint and preserve the bootstrap auth header instead of clearing
	// it as if the URL had rotated.
	var calls atomic.Int32
	// "http://Vault.TEST:80/cred" — equivalent to testProviderURL("/cred")
	// after canonicalisation (lowercased host, dropped default port).
	echoedURL := "http://Vault.TEST:80/cred"
	require.Equal(t, "vault.test", testProviderHost,
		"echoedURL and testProviderHost must canonicalise to the same value")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		require.Equal(t, "boot", r.Header.Get("X-Auth"),
			"bootstrap header should be preserved across equivalent URL echoes (call %d)", n)
		fmt.Fprintf(w, `{"value":"v%d","url":%q,"ttl":0}`, n, echoedURL)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     testProviderURL("/cred"),
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)
	installTestDialer(src, srv)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	require.Equal(t, int32(2), calls.Load())
}

func TestHttpSourceFactoryRejectsDuplicateHeadersCaseInsensitive(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type": "http",
		"url":  "https://vault.example.com/cred",
		"headers": map[string]string{
			"X-Auth": "one",
			"x-auth": "two",
		},
	}))
	require.ErrorContains(t, err, "duplicate header")
}

func intPtr(v int) *int { return &v }
