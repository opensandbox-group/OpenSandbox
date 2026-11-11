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

func TestIsSkipActivity(t *testing.T) {
	h := http.Header{}
	assert.False(t, IsSkipActivity(h), "absent header must not skip")

	h.Set(SkipActivity, SkipActivitySkipValue)
	assert.True(t, IsSkipActivity(h), "exact sentinel skips")

	h.Set(SkipActivity, "SKIP")
	assert.False(t, IsSkipActivity(h), "exact match only — case-sensitive sentinel")

	h.Set(SkipActivity, "yes")
	assert.False(t, IsSkipActivity(h), "unknown values are ignored (forward compatible)")
}

func TestSkipActivityHeaderSuppressesRecordingAndIsStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sentinel must never reach the backend: only the sweeper's
		// probe is issued with it, and backend apps must not learn it.
		assert.Empty(t, r.Header.Get(SkipActivity))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	prov := staticEndpointProvider{byID: map[string]sandbox.EndpointInfo{
		"skip": {UpstreamURL: upstream.URL},
	}}
	rec := &recordingActivity{}
	p := &Proxy{mode: ModeHeader, sandboxProvider: prov, activity: rec}

	// Without the header → activity recorded, request proxied.
	r1 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r1.Host = "skip-8080.sandbox.gw"
	p.ServeHTTP(httptest.NewRecorder(), r1)
	assert.Len(t, rec.snapshot(), 1)

	// With "OpenSandbox-Skip-Activity: skip" → no observation, request still proxied.
	r2 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r2.Host = "skip-8080.sandbox.gw"
	r2.Header.Set(SkipActivity, SkipActivitySkipValue)
	p.ServeHTTP(httptest.NewRecorder(), r2)
	assert.Len(t, rec.snapshot(), 1, "sentinel must suppress the activity observation")

	// Unknown value → forward compatible, activity still recorded.
	r3 := httptest.NewRequest(http.MethodGet, "http://x/status", nil)
	r3.Host = "skip-8080.sandbox.gw"
	r3.Header.Set(SkipActivity, "true")
	p.ServeHTTP(httptest.NewRecorder(), r3)
	assert.Len(t, rec.snapshot(), 2, "unknown values are ignored")
}
