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

package credentialvault

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func resolverTestInput() ResolveInput {
	return ResolveInput{
		Subject:     "sbx_01HZYEXAMPLE",
		Credential:  "spi-signer",
		Binding:     "internal-api",
		Destination: ResolveDestination{Scheme: "https", Authority: "api.internal.example.com:443", SNI: "api.internal.example.com"},
		Request:     ResolveRequestFrame{Method: "GET", Host: "api.internal.example.com", Path: "/v1/foo", Headers: map[string]string{"accept": "application/json"}},
	}
}

func requireKind(t *testing.T, err error, kind ResolverErrorKind) *ResolverError {
	t.Helper()
	require.Error(t, err)
	var resolverErr *ResolverError
	require.ErrorAs(t, err, &resolverErr)
	require.Equal(t, kind, resolverErr.Kind, "unexpected kind for error %v", err)
	return resolverErr
}

func TestParseResolverEndpoint(t *testing.T) {
	parsed, err := parseResolverEndpoint("uds:///run/opensandbox/resolver/resolve.sock")
	require.NoError(t, err)
	require.Equal(t, "unix", parsed.network)
	require.Equal(t, "/run/opensandbox/resolver/resolve.sock", parsed.socketPath)
	require.Equal(t, "/resolve", parsed.resolvePath)

	parsed, err = parseResolverEndpoint("unix:///tmp/r.sock")
	require.NoError(t, err)
	require.Equal(t, "unix", parsed.network)

	parsed, err = parseResolverEndpoint("https://resolver.internal:8443/base")
	require.NoError(t, err)
	require.Equal(t, "tcp", parsed.network)
	require.Equal(t, "resolver.internal:8443", parsed.authority)
	require.Equal(t, "https://resolver.internal:8443", parsed.origin)
	require.Equal(t, "/base/resolve", parsed.resolvePath)

	parsed, err = parseResolverEndpoint("https://resolver.internal")
	require.NoError(t, err)
	require.Equal(t, "resolver.internal", parsed.authority)
	require.Equal(t, "https://resolver.internal:443", parsed.origin)
	require.Equal(t, "/resolve", parsed.resolvePath)

	parsed, err = parseResolverEndpoint("https://RESOLVER.Internal:443")
	require.NoError(t, err)
	require.Equal(t, "https://resolver.internal:443", parsed.origin)

	for _, bad := range []string{
		"",
		"http://resolver.internal",
		"uds://host/path",
		"uds://relative.sock",
		"https:///nohost",
		"https://resolver.internal:8443/base?x=1",
		"https://resolver.internal:8443/base#frag",
		"https://user@resolver.internal:8443",
		"https://resolver.internal:8443/base/",
		"tcp://resolver.internal:8443",
	} {
		_, err = parseResolverEndpoint(bad)
		require.Error(t, err, "endpoint %q must be rejected", bad)
	}
}

func TestNormalizeResolverOrigin(t *testing.T) {
	origin, err := normalizeResolverOriginString("HTTPS://Resolver.Example.COM:8443")
	require.NoError(t, err)
	require.Equal(t, "https://resolver.example.com:8443", origin)

	origin, err = normalizeResolverOriginString("https://resolver.example.com")
	require.NoError(t, err)
	require.Equal(t, "https://resolver.example.com:443", origin)

	origin, err = normalizeResolverOriginString("https://[::1]:8443")
	require.NoError(t, err)
	require.Equal(t, "https://[::1]:8443", origin)

	_, err = normalizeResolverOriginString("http://resolver.example.com:8080")
	require.Error(t, err)
}

func TestParseResolverAuthEnv(t *testing.T) {
	tokens, err := ParseResolverAuthEnv("https://resolver.internal:8443=alpha, https://other.internal=beta ,,")
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"https://resolver.internal:8443": "alpha",
		"https://other.internal:443":     "beta",
	}, tokens)

	tokens, err = ParseResolverAuthEnv("")
	require.NoError(t, err)
	require.Empty(t, tokens)

	for _, bad := range []string{
		"resolver.internal=alpha",
		"https://resolver.internal:8443=",
		"=alpha",
		"notaentry",
		"https://resolver.internal:8443=alpha,https://resolver.internal:8443=beta",
	} {
		_, err = ParseResolverAuthEnv(bad)
		require.Error(t, err, "auth table %q must be rejected", bad)
	}
}

func TestResolverEnvDefaults(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverTimeout, "")
	t.Setenv(constants.EnvCredentialResolverMaxConcurrency, "")
	require.Equal(t, 2*time.Second, ResolverTimeoutFromEnv())
	require.Equal(t, constants.DefaultCredentialResolverMaxConcurrency, ResolverMaxConcurrencyFromEnv())

	t.Setenv(constants.EnvCredentialResolverTimeout, "5")
	t.Setenv(constants.EnvCredentialResolverMaxConcurrency, "8")
	require.Equal(t, 5*time.Second, ResolverTimeoutFromEnv())
	require.Equal(t, 8, ResolverMaxConcurrencyFromEnv())

	t.Setenv(constants.EnvCredentialResolverTimeout, "bogus")
	t.Setenv(constants.EnvCredentialResolverMaxConcurrency, "-3")
	require.Equal(t, 2*time.Second, ResolverTimeoutFromEnv())
	require.Equal(t, constants.DefaultCredentialResolverMaxConcurrency, ResolverMaxConcurrencyFromEnv())
}

func TestNewResolverClientFromEnv(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverAuth, "https://resolver.internal:8443=alpha")
	client, err := NewResolverClientFromEnv("https://resolver.internal:8443/base")
	require.NoError(t, err)
	require.NotNil(t, client)

	t.Setenv(constants.EnvCredentialResolverAuth, "https://resolver.internal:8443=")
	_, err = NewResolverClientFromEnv("https://resolver.internal:8443/base")
	require.Error(t, err)

	t.Setenv(constants.EnvCredentialResolverAuth, "")
	_, err = NewResolverClientFromEnv("https://resolver.internal:8443/base")
	require.NoError(t, err)
}

func TestMarshalResolveInputValidation(t *testing.T) {
	valid := resolverTestInput()
	_, err := marshalResolveInput(valid)
	require.NoError(t, err)

	cases := []func(*ResolveInput){
		func(i *ResolveInput) { i.Subject = "" },
		func(i *ResolveInput) { i.Credential = "" },
		func(i *ResolveInput) { i.Binding = "" },
		func(i *ResolveInput) { i.Destination.Scheme = "ftp" },
		func(i *ResolveInput) { i.Destination.Authority = "" },
		func(i *ResolveInput) { i.Destination.SNI = "" },
		func(i *ResolveInput) { i.Request.Method = "" },
		func(i *ResolveInput) { i.Request.Host = "" },
		func(i *ResolveInput) { i.Request.Path = "v1/foo" },
		func(i *ResolveInput) { i.Request.Headers = map[string]string{"x-a": ""} },
		func(i *ResolveInput) { i.Request.Headers = map[string]string{"": "v"} },
	}
	for _, mutate := range cases {
		input := valid
		mutate(&input)
		_, err = marshalResolveInput(input)
		requireKind(t, err, ResolverInvalidRequest)
	}
}

func TestDecodeResolveResponseStrictness(t *testing.T) {
	denied := []struct{ reason, body string }{
		{"header_name_too_long", `{"headers":{"` + strings.Repeat("n", 9000) + `":"v"}}`},
		{"header_value_too_long", `{"headers":{"x-a":"` + strings.Repeat("v", 9000) + `"}}`},
		{"denied_header_name", `{"headers":{"host":"evil"}}`},
		{"denied_header_name", `{"headers":{"bad name":"v"}}`},
		{"empty_value", `{"headers":{"x-a":""}}`},
		{"too_many_redactions", `{"headers":{"x-a":"v"},"redactions":{"x-a":[` + manyStrings(65) + `]}}`},
		{"redaction_value_too_long", `{"headers":{"x-a":"v"},"redactions":{"x-a":["` + strings.Repeat("v", 9000) + `"]}}`},
	}
	for _, testCase := range denied {
		_, err := decodeResolveResponse([]byte(testCase.body))
		resolverErr := requireKind(t, err, ResolverDenied)
		require.Equal(t, testCase.reason, resolverErr.Reason, "body %s", testCase.body)
	}

	invalid := []string{
		`not json`,
		`[]`,
		`{"headers":{}} extra`,
		`{"headers":{},"unknown":1}`,
		`{}`,
		`{"headers":null}`,
		`{"headers":[]}`,
		`{"headers":{"x-a":null}}`,
		`{"headers":{"x-a":1}}`,
		`{"headers":{"x-a":"v"},"redactions":[]}`,
		`{"headers":{"x-a":"v"},"redactions":{"x-a":[]}}`,
		`{"headers":{"x-a":"v"},"redactions":{"x-a":null}}`,
		`{"headers":{"x-a":"v"},"redactions":{"x-a":"v"}}`,
	}
	for _, body := range invalid {
		_, err := decodeResolveResponse([]byte(body))
		requireKind(t, err, ResolverInvalidResponse)
	}

	// Duplicate object keys are rejected, not last-wins.
	_, err := decodeResolveResponse([]byte(`{"headers":{"x-a":"1"},"headers":{"x-a":"2"}}`))
	requireKind(t, err, ResolverInvalidResponse)
	_, err = decodeResolveResponse([]byte(`{"headers":{"x-a":"1","x-a":"2"}}`))
	requireKind(t, err, ResolverInvalidResponse)

	document, err := decodeResolveResponse([]byte(`{"headers":{"X-A":"1","x-b":"2"},"redactions":{"X-A":["1"]}}`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"X-A": "1", "x-b": "2"}, document.headers)
	require.Equal(t, map[string][]string{"X-A": {"1"}}, document.redactions)

	// Redactions are optional.
	document, err = decodeResolveResponse([]byte(`{"headers":{"x-a":"1"}}`))
	require.NoError(t, err)
	require.Empty(t, document.redactions)
}

func TestNewResolverClientRequiresValidOptions(t *testing.T) {
	// Non-positive timeout/concurrency mean "use the configured defaults".
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: "uds:///tmp/r.sock", Timeout: -1, MaxConcurrency: -1})
	require.NoError(t, err)
	require.NotNil(t, client)
	_, err = NewResolverClient(ResolverClientOptions{Endpoint: "bogus", Timeout: time.Second, MaxConcurrency: 1})
	require.Error(t, err)
}

func TestResolveInputInvalidWithoutRoundTrip(t *testing.T) {
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: "uds:///nonexistent-egress-test.sock", Timeout: time.Second, MaxConcurrency: 1})
	require.NoError(t, err)
	input := resolverTestInput()
	input.Request.Path = "no-slash"
	_, err = client.Resolve(context.Background(), input)
	requireKind(t, err, ResolverInvalidRequest)
}

func TestResolveUnavailableWhenSocketMissing(t *testing.T) {
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: "uds:///nonexistent-egress-test.sock", Timeout: time.Second, MaxConcurrency: 1})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	requireKind(t, err, ResolverUnavailable)
}

// startEnvelopeServer runs an HTTPS test resolver answering every POST with
// status/body. It returns the endpoint URI and the test TLS configuration.
func startEnvelopeServer(t *testing.T, status int, body string, captured *capturedRequest) (string, *tls.Config) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		if len(raw) > 0 {
			_, _ = io.ReadFull(r.Body, raw)
		}
		if captured != nil {
			captured.record(r, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL, server.Client().Transport.(*http.Transport).TLSClientConfig
}

type capturedRequest struct {
	mu      sync.Mutex
	method  string
	path    string
	host    string
	auth    string
	content string
	body    map[string]any
}

func (c *capturedRequest) record(r *http.Request, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.path = r.URL.RequestURI()
	c.host = r.Host
	c.auth = r.Header.Get("Authorization")
	c.content = r.Header.Get("Content-Type")
	_ = json.Unmarshal(raw, &c.body)
}

func (c *capturedRequest) snapshot() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedRequest{method: c.method, path: c.path, host: c.host, auth: c.auth, content: c.content, body: c.body}
}

func TestHTTPSFrameAndHappyPath(t *testing.T) {
	var captured capturedRequest
	envelope := `{"headers":{"X-Test-Signature":"abc","authorization":"Bearer test-spi-signer"},"redactions":{"X-Test-Signature":["abc"]}}`
	endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, envelope, &captured)
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        endpoint + "/egress-resolver",
		Timeout:         time.Second,
		MaxConcurrency:  1,
		Auth:            map[string]string{endpoint: "secret-token"},
		TLSClientConfig: tlsConfig,
	})
	require.NoError(t, err)

	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)

	frame := captured.snapshot()
	require.Equal(t, http.MethodPost, frame.method)
	// D15: fixed framing — target is prefix + /resolve, Host is the authority.
	require.Equal(t, "/egress-resolver/resolve", frame.path)
	require.Equal(t, strings.TrimPrefix(endpoint, "https://"), frame.host)
	require.Equal(t, "application/json", frame.content)
	// D5: only the configured origin's token is presented.
	require.Equal(t, "Bearer secret-token", frame.auth)
	require.NotNil(t, frame.body)
	keys := make([]string, 0, len(frame.body))
	for key := range frame.body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"binding", "credential", "destination", "request", "subject"}, keys)

	require.Equal(t, map[string]string{"X-Test-Signature": "abc", "authorization": "Bearer test-spi-signer"}, decision.Headers)
	require.Equal(t, map[string]struct{}{"x-test-signature": {}, "authorization": {}}, decision.Suppress)
	require.Contains(t, decision.Redactions, "abc")
}

func TestUDSFrameAndNoAuthorization(t *testing.T) {
	dir, err := os.MkdirTemp("", "osbr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "r.sock")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	var captured capturedRequest
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		if len(raw) > 0 {
			_, _ = io.ReadFull(r.Body, raw)
		}
		captured.record(r, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"headers":{"x-a":"1"}}`))
	})}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})

	client, err := NewResolverClient(ResolverClientOptions{
		// D5: a UDS endpoint never carries auth — the socket is the boundary.
		Endpoint:       "uds://" + socketPath,
		Timeout:        time.Second,
		MaxConcurrency: 1,
		Auth:           map[string]string{"https://anything:443": "must-not-leak"},
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)

	frame := captured.snapshot()
	// D15: unix Host is localhost, target fixed at /resolve.
	require.Equal(t, "localhost", frame.host)
	require.Equal(t, "/resolve", frame.path)
	require.Empty(t, frame.auth, "UDS resolves must never carry an Authorization header")
}

func TestHTTPSUnconfiguredOriginSendsNoAuthorization(t *testing.T) {
	var captured capturedRequest
	endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, `{"headers":{"x-a":"1"}}`, &captured)
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        endpoint,
		Timeout:         time.Second,
		MaxConcurrency:  1,
		Auth:            map[string]string{"https://other-resolver.internal:8443": "global-token"},
		TLSClientConfig: tlsConfig,
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	require.Empty(t, captured.snapshot().auth, "an unmatched origin must never carry Authorization")
}

func TestResolverDeniedLimits(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{"tooManyHeaders", `{"headers":{` + manyHeaderEntries(maxResolverHeaderCount+1) + `}}`, "too_many_headers"},
		{"headerValueTooLong", `{"headers":{"x-a":"` + strings.Repeat("v", maxResolverValueBytes+1) + `"}}`, "header_value_too_long"},
		{"headerNameTooLong", `{"headers":{"` + strings.Repeat("n", maxResolverValueBytes+1) + `":"v"}}`, "header_name_too_long"},
		{"reservedHeaderName", `{"headers":{"proxy-authorization":"v"}}`, "denied_header_name"},
		{"reservedHeaderNameHost", `{"headers":{"Host":"evil"}}`, "denied_header_name"},
		{"invalidHeaderNameChars", `{"headers":{"bad name":"v"}}`, "denied_header_name"},
		{"emptyHeaderValue", `{"headers":{"x-a":""}}`, "empty_value"},
		{"tooManyRedactions", `{"headers":{"x-a":"v"},"redactions":{"x-a":[` + manyStrings(maxResolverRedactionCount+1) + `]}}`, "too_many_redactions"},
		{"redactionValueTooLong", `{"headers":{"x-a":"v"},"redactions":{"x-a":["` + strings.Repeat("v", maxResolverValueBytes+1) + `"]}}`, "redaction_value_too_long"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, testCase.body, nil)
			client, err := NewResolverClient(ResolverClientOptions{Endpoint: endpoint, Timeout: 2 * time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
			require.NoError(t, err)
			_, err = client.Resolve(context.Background(), resolverTestInput())
			resolverErr := requireKind(t, err, ResolverDenied)
			require.Equal(t, testCase.reason, resolverErr.Reason)
		})
	}
}

func TestResolverDeniedResponseTooLarge(t *testing.T) {
	// A 200 with a body over the 64KiB envelope cap is a per-request denial;
	// the client must also stop reading promptly.
	large := `{"headers":{"x-a":"` + strings.Repeat("v", maxResolverResponseBytes) + `"}}`
	endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, large, nil)
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: endpoint, Timeout: 5 * time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	resolverErr := requireKind(t, err, ResolverDenied)
	require.Equal(t, "response_too_large", resolverErr.Reason)
}

func TestResolverInvalidAndRejectedClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   ResolverErrorKind
	}{
		{"badJSON", http.StatusOK, "not-json{", ResolverInvalidResponse},
		{"unknownField", http.StatusOK, `{"headers":{},"extra":1}`, ResolverInvalidResponse},
		{"wrongContentType", http.StatusOK, `{"headers":{}}`, ResolverInvalidResponse},
		{"serverError", http.StatusServiceUnavailable, `{"error":"resolution_refused"}`, ResolverRejected},
		{"unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, ResolverRejected},
		{"notFound", http.StatusNotFound, `{"error":"not_found"}`, ResolverRejected},
		{"redirectNotFollowed", http.StatusFound, "", ResolverRejected},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var captured capturedRequest
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw := make([]byte, r.ContentLength)
				if len(raw) > 0 {
					_, _ = io.ReadFull(r.Body, raw)
				}
				captured.record(r, raw)
				if testCase.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/plain")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			server.StartTLS()
			defer server.Close()
			tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig
			client, err := NewResolverClient(ResolverClientOptions{Endpoint: server.URL, Timeout: 2 * time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
			require.NoError(t, err)
			_, err = client.Resolve(context.Background(), resolverTestInput())
			resolverErr := requireKind(t, err, testCase.kind)
			if testCase.kind == ResolverRejected {
				require.Equal(t, testCase.status, resolverErr.Status)
			}
		})
	}
}

func TestResolveTimeoutClass(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"headers":{"x-a":"1"}}`))
	}))
	server.StartTLS()
	defer server.Close()
	defer close(release)
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig

	client, err := NewResolverClient(ResolverClientOptions{Endpoint: server.URL, Timeout: 100 * time.Millisecond, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
	require.NoError(t, err)

	start := time.Now()
	_, err = client.Resolve(context.Background(), resolverTestInput())
	requireKind(t, err, ResolverTimeout)
	require.Less(t, time.Since(start), time.Second)
}

func TestResolveCallerDeadlineHonored(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"headers":{"x-a":"1"}}`))
	}))
	server.StartTLS()
	defer server.Close()
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: server.URL, Timeout: 10 * time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.Resolve(ctx, resolverTestInput())
	requireKind(t, err, ResolverTimeout)
}

func TestResolverMaxConcurrencyBounded(t *testing.T) {
	var mu sync.Mutex
	var inflight, maxInflight int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"headers":{"x-a":"1"}}`))
	}))
	server.StartTLS()
	defer server.Close()
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig

	client, err := NewResolverClient(ResolverClientOptions{Endpoint: server.URL, Timeout: 5 * time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
	require.NoError(t, err)

	const calls = 4
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, errs[slot] = client.Resolve(context.Background(), resolverTestInput())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, maxInflight, "MaxConcurrency=1 must serialize resolves")
}

func TestResolverClientInterfaceContract(t *testing.T) {
	// The decision shape is the interception contract: headers as returned,
	// lowercase suppress names, canonically ordered redactions.
	envelope := `{"headers":{"X-Signature":"sig","X-Token":"tok"},"redactions":{"X-Signature":["sig"]}}`
	endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, envelope, nil)
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: endpoint, Timeout: time.Second, MaxConcurrency: 1, TLSClientConfig: tlsConfig})
	require.NoError(t, err)
	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"X-Signature": "sig", "X-Token": "tok"}, decision.Headers)
	require.Equal(t, map[string]struct{}{"x-signature": {}, "x-token": {}}, decision.Suppress)
	require.True(t, slices.Equal(canonicalResolverRedactions(decision.Redactions), decision.Redactions))
	require.Contains(t, decision.Redactions, "sig")
	require.Contains(t, decision.Redactions, "tok")
}

func manyHeaderEntries(count int) string {
	entries := make([]string, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, fmt.Sprintf(`"x-h%d":"v"`, i))
	}
	return strings.Join(entries, ",")
}

func manyStrings(count int) string {
	values := make([]string, 0, count)
	for i := 0; i < count; i++ {
		values = append(values, fmt.Sprintf(`"s%d"`, i))
	}
	return strings.Join(values, ",")
}
