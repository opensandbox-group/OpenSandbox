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
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

// handleProxyAuthorize is served only on the mitmproxy-owned Unix socket.
// Never resolve the destination here: the upstream proxy owns destination DNS.
func (s *policyServer) handleProxyAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := strings.TrimSuffix(strings.ToLower(r.URL.Query().Get("host")), ".")
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 || host == "" {
		http.Error(w, "invalid destination", http.StatusBadRequest)
		return
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if _, err := hostselector.ParseCanonical(host); err != nil || strings.HasPrefix(host, "*.") {
			http.Error(w, "invalid destination", http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The experimental revision runtime does not support chained proxies.
	if s.liveRevision {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	if port == 853 || port == 443 && constants.IsTruthy(os.Getenv(constants.EnvBlockDoH443)) ||
		!s.effectivePolicy().AllowsProxyDestination(host, port) {
		http.Error(w, "destination denied", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
