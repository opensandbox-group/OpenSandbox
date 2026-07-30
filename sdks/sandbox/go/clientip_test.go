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

package opensandbox

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// withStubbedClientIP replaces the cached client-IP detection with a fixed
// value for the duration of the test and restores it afterwards.
func withStubbedClientIP(t *testing.T, ip string) {
	t.Helper()
	prevDetector := clientIPDetector
	prevValue := clientIPValue
	clientIPDetector = func() string { return ip }
	clientIPOnce = sync.Once{}
	clientIPValue = ""
	t.Cleanup(func() {
		clientIPDetector = prevDetector
		clientIPValue = prevValue
		// Reset so subsequent callers re-detect via the restored detector.
		clientIPOnce = sync.Once{}
	})
}

func TestApplyClientIP_SetsHeaderWhenAbsent(t *testing.T) {
	got := applyClientIP(nil, "10.1.2.3")
	require.Equal(t, "10.1.2.3", got[clientIPHeader])
}

func TestApplyClientIP_DoesNotOverwriteUserValue(t *testing.T) {
	// User-provided header using a different case must be preserved.
	headers := map[string]string{"open-sandbox-client-ip": "192.168.0.9"}
	got := applyClientIP(headers, "10.1.2.3")
	require.Equal(t, "192.168.0.9", got["open-sandbox-client-ip"])
	_, hasCanonical := got[clientIPHeader]
	require.True(t, !hasCanonical, "must not add a second client-IP header key")
}

func TestApplyClientIP_EmptyIPIsNoOp(t *testing.T) {
	require.Len(t, applyClientIP(nil, ""), 0)

	headers := map[string]string{"X-Foo": "bar"}
	got := applyClientIP(headers, "")
	_, has := got[clientIPHeader]
	require.True(t, !has, "must not set header when IP is empty")
}

func TestDetectOutboundIP_ReturnsValidOrEmpty(t *testing.T) {
	// Best-effort: may be empty in a network-less environment, but if non-empty
	// it must be a valid, non-loopback, non-unspecified IP.
	ip := detectOutboundIP()
	if ip == "" {
		return
	}
	parsed := net.ParseIP(ip)
	require.NotNil(t, parsed, "detected value %q is not a valid IP", ip)
	require.True(t, !parsed.IsLoopback(), "detected loopback IP %q", ip)
	require.True(t, !parsed.IsUnspecified(), "detected unspecified IP %q", ip)
}

func TestSelectClientIP_PrefersNamedNICAndSkipsVirtual(t *testing.T) {
	nics := []nicAddrs{
		{name: "docker0", ips: []net.IP{net.ParseIP("172.17.0.1")}}, // virtual, skip
		{name: "utun3", ips: []net.IP{net.ParseIP("10.8.0.3")}},     // VPN, skip
		{name: "en0", ips: []net.IP{net.ParseIP("10.1.1.1")}},       // main NIC
		{name: "eth1", ips: []net.IP{net.ParseIP("192.168.5.5")}},
	}
	require.Equal(t, "10.1.1.1", selectClientIP(nics))
}

func TestSelectClientIP_FallsBackToPrivateWhenNoNamedNIC(t *testing.T) {
	// No en*/eth*/bond* names -> pick a usable private IPv4, skipping virtual.
	nics := []nicAddrs{
		{name: "docker0", ips: []net.IP{net.ParseIP("172.17.0.1")}}, // virtual, skip
		{name: "custom9", ips: []net.IP{net.ParseIP("8.8.4.4")}},    // public, lower pref
		{name: "wlan9", ips: []net.IP{net.ParseIP("10.0.0.50")}},    // private, preferred
	}
	require.Equal(t, "10.0.0.50", selectClientIP(nics))
}

func TestSelectClientIP_SkipsLoopbackAndLinkLocal(t *testing.T) {
	nics := []nicAddrs{
		{name: "eth0", ips: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("169.254.1.1"), net.ParseIP("10.1.2.3")}},
	}
	require.Equal(t, "10.1.2.3", selectClientIP(nics))
}

func TestSelectClientIP_ReturnsEmptyWhenNoUsable(t *testing.T) {
	nics := []nicAddrs{
		{name: "docker0", ips: []net.IP{net.ParseIP("172.17.0.1")}},
		{name: "eth0", ips: []net.IP{net.ParseIP("169.254.1.1")}},
	}
	require.Equal(t, "", selectClientIP(nics))
}

func TestNewClient_SendsClientIPHeader(t *testing.T) {
	withStubbedClientIP(t, "10.9.8.7")

	var gotIP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = r.Header.Get(clientIPHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "OPEN-SANDBOX-API-KEY")
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "10.9.8.7", gotIP)
}

func TestNewClient_OmitsClientIPHeaderWhenUndetectable(t *testing.T) {
	withStubbedClientIP(t, "")

	present := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header[http.CanonicalHeaderKey(clientIPHeader)]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "OPEN-SANDBOX-API-KEY")
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	require.True(t, !present, "client IP header must be absent when undetectable")
}
