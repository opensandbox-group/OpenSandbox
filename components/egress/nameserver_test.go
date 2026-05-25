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

package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowIPsForNft_EmptyResolv(t *testing.T) {
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(resolv, []byte("# empty\n"), 0644))
	ips := AllowIPsForNft(resolv)
	require.Len(t, ips, 1, "expected 1 IP (127.0.0.1)")
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), ips[0])
}

func TestAllowIPsForNft_ValidNameservers(t *testing.T) {
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	// Standard resolv.conf with two nameservers
	content := "nameserver 192.168.65.7\nnameserver 10.0.0.1\n"
	require.NoError(t, os.WriteFile(resolv, []byte(content), 0644))
	ips := AllowIPsForNft(resolv)
	require.Len(t, ips, 3, "expected 3 IPs (127.0.0.1 + 2 nameservers)")
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), ips[0], "expected first 127.0.0.1")
	require.Equal(t, netip.MustParseAddr("192.168.65.7"), ips[1], "expected 192.168.65.7")
	require.Equal(t, netip.MustParseAddr("10.0.0.1"), ips[2], "expected 10.0.0.1")
}

func TestAllowIPsForNft_FiltersInvalid(t *testing.T) {
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	// 0.0.0.0 and 127.0.0.11 should be filtered; 192.168.1.1 kept
	content := "nameserver 0.0.0.0\nnameserver 192.168.1.1\nnameserver 127.0.0.11\n"
	require.NoError(t, os.WriteFile(resolv, []byte(content), 0644))
	ips := AllowIPsForNft(resolv)
	require.Len(t, ips, 2, "expected 2 IPs (127.0.0.1 + 192.168.1.1)")
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), ips[0], "expected first 127.0.0.1")
	require.Equal(t, netip.MustParseAddr("192.168.1.1"), ips[1], "expected 192.168.1.1")
}

func TestAllowIPsForNft_Cap(t *testing.T) {
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	var lines []string
	for i := 1; i <= 11; i++ {
		lines = append(lines, fmt.Sprintf("nameserver 10.0.0.%d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(resolv, []byte(content), 0644))

	ips := AllowIPsForNft(resolv)
	// 127.0.0.1 + first 10 nameservers (fixed cap)
	require.Len(t, ips, 11, "expected 11 IPs (127.0.0.1 + 10 from resolv)")
	require.Equal(t, netip.MustParseAddr("10.0.0.1"), ips[1], "expected first nameserver to be 10.0.0.1")
	require.Equal(t, netip.MustParseAddr("10.0.0.10"), ips[10], "expected tenth nameserver to be 10.0.0.10")
}

func TestIsValidNameserverIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"0.0.0.0", false},
		{"::", false},
		{"127.0.0.1", false},
		{"127.0.0.11", false},
		{"::1", false},
		{"192.168.65.7", true},
		{"10.0.0.1", true},
		{"8.8.8.8", true},
	}
	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.ip)
		got := isValidNameserverIP(ip)
		if got != tt.want {
			t.Errorf("isValidNameserverIP(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}
