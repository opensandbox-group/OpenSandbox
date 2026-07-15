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

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/stretchr/testify/assert"
)

// routeTestProvider stubs sandbox lookup for routing tests (non-empty access-token annotation).
type routeTestProvider struct{}

func (routeTestProvider) GetEndpoint(string) (*sandbox.EndpointInfo, error) {
	return &sandbox.EndpointInfo{
		Endpoint:          "127.0.0.1",
		SecureAccessToken: "annot-secret",
	}, nil
}

func (routeTestProvider) Start(context.Context) error { return nil }

func TestGetSandboxHostDefinition_HeaderSecureSig(t *testing.T) {
	secret := []byte("ingress-test-secret")
	sb := "gamma"
	port := 7777
	e := strconv.FormatUint(uint64(time.Now().Add(1*time.Hour).Unix()), 36)
	sig := signature.ExpectedHex8(signature.Inner(secret, signature.CanonicalBytes(sb, port, e))) + "k"

	p := &Proxy{
		mode:            ModeHeader,
		sandboxProvider: routeTestProvider{},
		secure:          &signature.Verifier{Keys: map[string][]byte{"k": secret}},
	}
	label := fmt.Sprintf("%s-%d-%s-%s", sb, port, e, sig)
	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = label + ".gw.example.com"
	host, code, err := p.getSandboxHostDefinition(r)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, sb, host.ingressKey)
	assert.Equal(t, port, host.port)
}

func TestGetSandboxHostDefinition_HeaderSecureHeaderBypassSignedShape(t *testing.T) {
	sb := "gamma"
	port := 7777
	// 9 characters: 8-hex + 1 key id (header bypasses HMAC, but must parse as OSEP-0011)
	sig := "aabbccdd" + "1"
	e := "0"
	label := fmt.Sprintf("%s-%d-%s-%s", sb, port, e, sig)

	p := &Proxy{mode: ModeHeader, sandboxProvider: routeTestProvider{}}
	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = label + ".gw.example.com"
	r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "annot-secret")
	host, code, err := p.getSandboxHostDefinition(r)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, sb, host.ingressKey)
	assert.Equal(t, port, host.port)
}

func TestGetSandboxHostDefinition_HeaderSecureHeaderBypassLegacy(t *testing.T) {
	p := &Proxy{mode: ModeHeader, sandboxProvider: routeTestProvider{}}
	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = "mysb-9090.example.com"
	r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "annot-secret")
	host, code, err := p.getSandboxHostDefinition(r)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "mysb", host.ingressKey)
	assert.Equal(t, 9090, host.port)
}

func TestGetSandboxHostDefinition_URISecureSig(t *testing.T) {
	secret := []byte("uri-mode-secret")
	sb := "d-e"
	port := 3000
	e := strconv.FormatUint(uint64(time.Now().Add(1*time.Hour).Unix()), 36)
	sig := signature.ExpectedHex8(signature.Inner(secret, signature.CanonicalBytes(sb, port, e))) + "a"

	p := &Proxy{
		mode:            ModeURI,
		sandboxProvider: routeTestProvider{},
		secure:          &signature.Verifier{Keys: map[string][]byte{"a": secret}},
	}
	path := fmt.Sprintf("/%s/%d/%s/%s/api/x", sb, port, e, sig)
	r := httptest.NewRequest(http.MethodGet, "http://ingress.local"+path, nil)
	host, code, err := p.getSandboxHostDefinition(r)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, sb, host.ingressKey)
	assert.Equal(t, port, host.port)
	assert.Equal(t, "/api/x", host.requestURI)
}

func TestGetSandboxHostDefinition_URISecureHeaderBypass(t *testing.T) {
	sb := "d-e"
	port := 3000
	e := "0"
	sig := "cafebabe" + "0"

	p := &Proxy{mode: ModeURI, sandboxProvider: routeTestProvider{}}
	path := fmt.Sprintf("/%s/%d/%s/%s/api/x", sb, port, e, sig)
	r := httptest.NewRequest(http.MethodGet, "http://ingress.local"+path, nil)
	r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "annot-secret")
	host, code, err := p.getSandboxHostDefinition(r)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, sb, host.ingressKey)
	assert.Equal(t, port, host.port)
	assert.Equal(t, "/api/x", host.requestURI)
}
