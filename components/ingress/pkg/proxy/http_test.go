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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/stretchr/testify/assert"
)

// mockProvider implements sandbox.Provider interface for testing
type mockProvider struct {
	endpoints   map[string]string // sandboxName -> IP
	notReady    map[string]bool   // sandboxName -> notReady flag
	accessToken map[string]string // sandboxName -> opensandbox.io/secure-access-token value (non-empty => verification required)
}

func (m *mockProvider) sandboxExists(sandboxId string) bool {
	if m.notReady != nil && m.notReady[sandboxId] {
		return true
	}
	_, ok := m.endpoints[sandboxId]
	return ok
}

func (m *mockProvider) GetEndpoint(sandboxId string) (*sandbox.EndpointInfo, error) {
	if m.notReady != nil && m.notReady[sandboxId] {
		return nil, fmt.Errorf("%w: %s", sandbox.ErrSandboxNotReady, sandboxId)
	}
	if !m.sandboxExists(sandboxId) {
		return nil, fmt.Errorf("%w: %s", sandbox.ErrSandboxNotFound, sandboxId)
	}
	ip := m.endpoints[sandboxId]
	token := ""
	if m.accessToken != nil {
		token = strings.TrimSpace(m.accessToken[sandboxId])
	}
	if ip == "" {
		return nil, fmt.Errorf("%w: %s", sandbox.ErrSandboxNotFound, sandboxId)
	}
	return &sandbox.EndpointInfo{
		Endpoint:          ip,
		SecureAccessToken: token,
	}, nil
}

func (m *mockProvider) Start(_ context.Context) error {
	return nil
}

func Test_HTTPProxy(t *testing.T) {
	t.Run("with header mode", func(t *testing.T) {
		httpProxyWithHeaderMode(t)
	})

	t.Run("with uri mode", func(t *testing.T) {
		httpProxyWithURIMode(t)
	})
}

func httpProxyWithHeaderMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(realBackendHTTPHandler))
	defer server.Close()
	serverPort := server.URL[len("http://127.0.0.1:"):]

	// Create mock provider with sandbox endpoint
	provider := &mockProvider{
		endpoints: map[string]string{
			"test-sandbox": "127.0.0.1",
		},
	}

	ctx := context.Background()
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})
	proxy := NewProxy(ctx, provider, ModeHeader, nil, nil)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	port, err := findAvailablePort()
	assert.Nil(t, err)

	go func() {
		assert.NoError(t, http.ListenAndServe(":"+strconv.Itoa(port), mux))
	}()

	time.Sleep(2 * time.Second)

	// no header
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/hello", port), nil)
	assert.Nil(t, err)
	response, err := http.DefaultClient.Do(request)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	bytes, _ := io.ReadAll(response.Body)
	t.Log(string(bytes))

	// no sandbox backend
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/hello", port), nil)
	request.Header.Set(SandboxIngress, fmt.Sprintf("non-existent-%v", port))
	response, err = http.DefaultClient.Do(request)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode) // ErrSandboxNotFound -> 404
	bytes, _ = io.ReadAll(response.Body)
	t.Log(string(bytes))

	// valid sandbox request
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/hello?a=1&b=2", port), nil)
	assert.Nil(t, err)

	request.Header.Set(SandboxIngress, fmt.Sprintf("test-sandbox-%v", serverPort))
	response, err = http.DefaultClient.Do(request)
	assert.Nil(t, err)
	if response.StatusCode != http.StatusOK {
		bytes, err := io.ReadAll(response.Body)
		assert.Nil(t, err)
		t.Log(string(bytes))
	}
	assert.Equal(t, http.StatusOK, response.StatusCode)

	// Compatible Host parsing for reverse proxy mode
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/hello?a=1&b=2", port), nil)
	assert.Nil(t, err)

	request.Host = fmt.Sprintf("test-sandbox-%v.sandbox.alibaba-inc.com", serverPort)
	response, err = http.DefaultClient.Do(request)
	assert.Nil(t, err)
	if response.StatusCode != http.StatusOK {
		bytes, err := io.ReadAll(response.Body)
		assert.Nil(t, err)
		t.Log(string(bytes))
	}
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func httpProxyWithURIMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(realBackendHTTPHandler))
	defer server.Close()
	serverPort := server.URL[len("http://127.0.0.1:"):]

	// Create mock provider with sandbox endpoint
	provider := &mockProvider{
		endpoints: map[string]string{
			"test-sandbox": "127.0.0.1",
		},
	}

	ctx := context.Background()
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})
	proxy := NewProxy(ctx, provider, ModeURI, nil, nil)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	port, err := findAvailablePort()
	assert.Nil(t, err)

	go func() {
		assert.NoError(t, http.ListenAndServe(":"+strconv.Itoa(port), mux))
	}()

	time.Sleep(2 * time.Second)

	// uri is empty
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v", port), nil)
	assert.Nil(t, err)
	response, err := http.DefaultClient.Do(request)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	bytes, _ := io.ReadAll(response.Body)
	t.Log(string(bytes))

	// no sandbox backend
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/non-existent-xxx/80/hello", port), nil)
	response, err = http.DefaultClient.Do(request)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode) // ErrSandboxNotFound -> 404
	bytes, _ = io.ReadAll(response.Body)
	t.Log(string(bytes))

	// valid sandbox request
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%v/test-sandbox/%v/hello?a=1&b=2", port, serverPort), nil)
	assert.Nil(t, err)
	response, err = http.DefaultClient.Do(request)
	assert.Nil(t, err)
	if response.StatusCode != http.StatusOK {
		bytes, err := io.ReadAll(response.Body)
		assert.Nil(t, err)
		t.Log(string(bytes))
	}
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func realBackendHTTPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != "/hello" {
		http.Error(w, fmt.Sprintf("path is not /hello, but %s", r.URL.Path), http.StatusBadRequest)
	}
	if r.URL.RawQuery != "a=1&b=2" {
		http.Error(w, fmt.Sprintf("query is not a=1&b=2, but %s", r.URL.RawQuery), http.StatusBadRequest)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("hello world"))
}
