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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestHandleDelete_DropEstablishedPreservesAllowedSingletonPort(t *testing.T) {
	for _, target := range []string{"192.0.2.7", "2001:db8::7"} {
		addr := netip.MustParseAddr(target)
		subnet := netip.PrefixFrom(addr, addr.BitLen()-1).Masked().String()
		t.Run(target, func(t *testing.T) {
			initial, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"allow","target":"` + target + `","ports":[443,8443]},{"action":"deny","target":"` + subnet + `","ports":[443]}]}`)
			require.NoError(t, err)
			runner := new(policyPortNFTRunner)
			m := nftables.NewManagerWithRunner(runner.run)
			require.NoError(t, m.ApplyStatic(t.Context(), initial))
			proxy := &stubProxy{updated: initial}
			srv := &policyServer{proxy: proxy, nft: m, enforcementMode: "dns+nft"}
			before := runner.count()
			w := httptest.NewRecorder()
			srv.handlePolicy(w, httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", strings.NewReader(`["`+target+`"]`)))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Greater(t, runner.count(), before)
			require.Len(t, proxy.updated.Egress, 1)
			require.Equal(t, subnet, proxy.updated.Egress[0].Target)
			var resp policyStatusResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			require.NotNil(t, resp.DroppedEstablished)
			script := runner.last(t)
			family := "ip6"
			if addr.Is4() {
				family = "ip"
			}
			require.Contains(t, script, family+" daddr "+subnet+" tcp dport { 443 } ct state established,related drop")
			for _, line := range strings.Split(script, "\n") {
				if strings.Contains(line, "ct state established,related drop") {
					require.NotContains(t, line, "8443", "DELETE must leave the admitted port alone")
				}
			}
		})
	}
}

func TestHandleDelete_DropEstablishedQueryPassesLearnedIPs(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	nft := &stubNft{
		domains: map[string][]netip.Addr{
			"deb.debian.org": {netip.MustParseAddr("203.0.113.10")},
		},
		dropMatched: 2,
	}
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		nft:             nft,
		enforcementMode: "dns+nft",
	}

	req := httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", strings.NewReader(`["deb.debian.org"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nft.lastDrops.Addrs)

	var resp policyStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.DroppedEstablished)
	require.Equal(t, 2, *resp.DroppedEstablished)
}

func TestHandleDelete_WithoutDropEstablishedLeavesConntrackAlone(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	nft := &stubNft{
		domains: map[string][]netip.Addr{
			"deb.debian.org": {netip.MustParseAddr("203.0.113.10")},
		},
	}
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		nft:             nft,
		enforcementMode: "dns+nft",
	}

	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(`["deb.debian.org"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, nft.lastDrops.Empty())
	require.Equal(t, 1, nft.calls)

	var resp policyStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.DroppedEstablished)
}

func TestDropEstablishedRequested(t *testing.T) {
	require.True(t, dropEstablishedRequested(httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", nil)))
	require.True(t, dropEstablishedRequested(httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=1", nil)))
	require.True(t, dropEstablishedRequested(httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=y", nil)), "must match constants.IsTruthy")
	require.False(t, dropEstablishedRequested(httptest.NewRequest(http.MethodDelete, "/policy", nil)))
	require.False(t, dropEstablishedRequested(httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=false", nil)))
}

func TestCommitPolicy_DropEstablishedUnsupportedWithUpstreamProxy(t *testing.T) {
	t.Setenv("OPENSANDBOX_EGRESS_UPSTREAM_PROXY", "http://proxy.example:8080")
	t.Setenv("OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT", "true")
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	nft := &stubNft{}
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		nft:             nft,
		enforcementMode: "dns+nft",
	}
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	ok, dropped := srv.commitPolicy(context.Background(), w, newPol, "post", true)
	require.False(t, ok)
	require.Nil(t, dropped)
	require.Equal(t, http.StatusNotImplemented, w.Code)
	require.Contains(t, w.Body.String(), "UPSTREAM_PROXY")
	require.Equal(t, 0, nft.calls)
}

func TestCommitPolicy_DropEstablishedUnsupportedWithRevisionRuntime(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)
	nft := &stubNft{}
	srv := &policyServer{
		proxy:            &stubProxy{updated: initial},
		nft:              nft,
		enforcementMode:  "dns+nft",
		revisionRecovery: &revisionRecoveryState{current: &effectivePolicyBase{inputs: effectivePolicyInputs{user: initial}}},
	}
	w := httptest.NewRecorder()
	ok, _ := srv.commitPolicy(context.Background(), w, initial, "post", true)
	require.False(t, ok)
	require.Equal(t, http.StatusNotImplemented, w.Code)
	require.Contains(t, w.Body.String(), "revision runtime")
	require.Equal(t, 0, nft.calls)
}

func TestCommitPolicy_DropEstablishedUnsupportedWithoutNft(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	proxy := &stubProxy{updated: initial}
	srv := &policyServer{
		proxy:           proxy,
		enforcementMode: "dns",
	}

	req := httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", strings.NewReader(`["deb.debian.org"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "dns+nft")
	require.Same(t, initial, proxy.updated, "policy must not change on 501")
}

func TestHandleDelete_DropEstablishedNoMatchReportsZero(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	nft := &stubNft{}
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		nft:             nft,
		enforcementMode: "dns+nft",
	}

	req := httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", strings.NewReader(`["absent.example.com"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 0, nft.calls)

	var resp policyStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.DroppedEstablished, "field must be present whenever dropEstablished was set")
	require.Equal(t, 0, *resp.DroppedEstablished)
}

func TestHandleDelete_DropEstablishedNoMatchStillHonors501(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		enforcementMode: "dns",
	}
	req := httptest.NewRequest(http.MethodDelete, "/policy?dropEstablished=true", strings.NewReader(`["absent.example.com"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
}

func TestHandlePost_IgnoresDropEstablished(t *testing.T) {
	initial, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	nft := &stubNft{
		domains: map[string][]netip.Addr{
			"deb.debian.org": {netip.MustParseAddr("203.0.113.10")},
		},
		dropMatched: 2,
	}
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		nft:             nft,
		enforcementMode: "dns+nft",
	}

	req := httptest.NewRequest(http.MethodPost, "/policy?dropEstablished=true", strings.NewReader(`{"defaultAction":"deny","egress":[]}`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, nft.lastDrops.Empty(), "POST is outside the documented dropEstablished surface")

	var resp policyStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.DroppedEstablished)
}
