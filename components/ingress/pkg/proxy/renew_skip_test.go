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

package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/stretchr/testify/assert"
)

type recordingPublisher struct {
	calls []string
}

func (p *recordingPublisher) PublishIntent(namespace, sandboxID string, port int, requestURI string) {
	p.calls = append(p.calls, sandboxID)
}

func TestIsAccessRenewSkip(t *testing.T) {
	h := http.Header{}
	assert.False(t, IsAccessRenewSkip(h), "absent header must not skip")

	h.Set(AccessRenew, AccessRenewSkipValue)
	assert.True(t, IsAccessRenewSkip(h), "exact sentinel skips")

	h.Set(AccessRenew, "SKIP")
	assert.False(t, IsAccessRenewSkip(h), "exact match only — case-sensitive sentinel")

	h.Set(AccessRenew, "true")
	assert.False(t, IsAccessRenewSkip(h), "unknown values are ignored (forward compatible)")
}

func TestRenewSkipHeaderSuppressesIntentAndIsStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The control header must never reach the backend.
		assert.Empty(t, r.Header.Get(AccessRenew))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	prov := staticEndpointProvider{byID: map[string]sandbox.EndpointInfo{
		"renew": {Endpoint: upstream.Listener.Addr().String()},
	}}
	pub := &recordingPublisher{}
	p := &Proxy{mode: ModeHeader, sandboxProvider: prov, renewIntentPublisher: pub}

	// Without the header → intent published, request proxied.
	r1 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r1.Host = "renew-8080.sandbox.gw"
	p.ServeHTTP(httptest.NewRecorder(), r1)
	assert.Len(t, pub.calls, 1)

	// With "OpenSandbox-Access-Renew: skip" → no additional intent, request still proxied.
	r2 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r2.Host = "renew-8080.sandbox.gw"
	r2.Header.Set(AccessRenew, AccessRenewSkipValue)
	p.ServeHTTP(httptest.NewRecorder(), r2)
	assert.Len(t, pub.calls, 1, "skip header must suppress the renew intent")

	// Unknown value → forward compatible, intent still published.
	r3 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r3.Host = "renew-8080.sandbox.gw"
	r3.Header.Set(AccessRenew, "later")
	p.ServeHTTP(httptest.NewRecorder(), r3)
	assert.Len(t, pub.calls, 2, "unknown values are ignored")
}
