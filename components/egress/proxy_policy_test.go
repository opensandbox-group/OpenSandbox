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

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestProxyAuthorizeCurrentPolicy(t *testing.T) {
	p, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"code.invalid"}]}`)
	require.NoError(t, err)
	proxy := &stubProxy{updated: p}
	s := &policyServer{proxy: proxy}
	check := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleProxyAuthorize(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, want, w.Code, w.Body.String())
	}
	check("/egress/authorize?host=code.invalid&port=443", http.StatusNoContent)
	check("/egress/authorize?host=blocked.invalid&port=443", http.StatusForbidden)
	check("/egress/authorize?host=code.invalid&port=853", http.StatusForbidden)
	check("/egress/authorize?host=*.invalid&port=443", http.StatusBadRequest)
	check("/egress/authorize?host=code.invalid&port=0", http.StatusBadRequest)
	check("/egress/authorize?host=code.invalid", http.StatusBadRequest)
	proxy.UpdatePolicy(policy.DefaultDenyPolicy())
	check("/egress/authorize?host=code.invalid&port=443", http.StatusForbidden)
	proxy.UpdatePolicy(p)
	t.Setenv(constants.EnvBlockDoH443, "true")
	check("/egress/authorize?host=code.invalid&port=443", http.StatusForbidden)
	s.liveRevision = true
	check("/egress/authorize?host=code.invalid&port=80", http.StatusServiceUnavailable)
}
