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

package mitmproxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseUpstreamProxyValid(t *testing.T) {
	tests := []struct {
		raw    string
		scheme string
		host   string
		port   int
	}{
		{"http://proxy.example.com:3128", "http", "proxy.example.com", 3128},
		{"https://proxy.example.com:8443", "https", "proxy.example.com", 8443},
		{"http://proxy.example.com", "http", "proxy.example.com", 80},
		{"https://proxy.example.com", "https", "proxy.example.com", 443},
		{"http://10.0.0.1:3128", "http", "10.0.0.1", 3128},
		{"http://[fd00::1]:3128", "http", "fd00::1", 3128},
		{"  http://proxy.example.com:3128  ", "http", "proxy.example.com", 3128},
	}
	for _, tc := range tests {
		spec, err := parseUpstreamProxy(tc.raw)
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.scheme, spec.Scheme, tc.raw)
		require.Equal(t, tc.host, spec.Host, tc.raw)
		require.Equal(t, tc.port, spec.Port, tc.raw)
	}
}

func TestParseUpstreamProxyRejectsInvalid(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"proxy.example.com:3128",          // missing scheme
		"socks5://proxy.example.com:1080", // unsupported scheme
		"http://",                         // missing host
		"http:///path",                    // missing host
		"http://user:pass@proxy:3128",     // userinfo must not carry credentials
		"http://proxy:3128?x=1",           // query not meaningful for CONNECT
		"http://proxy:3128#frag",          // fragment not meaningful
		"http://proxy:notaport",           // invalid port
		"http://proxy:0",                  // port out of range
		"http://proxy:70000",              // port out of range
	}
	for _, raw := range tests {
		_, err := parseUpstreamProxy(raw)
		require.Error(t, err, raw)
	}
}
