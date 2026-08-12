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

//go:build linux

package nftables

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadTCPConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tcp")
	contents := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1234 01010101:01BB 01 00000000:00000000 00:00000000 00000000  1000 0 1\n" +
		"   1: 0100007F:1235 02020202:01BB 06 00000000:00000000 00:00000000 00000000  1000 0 2\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	connections, err := readTCPConnections(context.Background(), path, false)
	require.NoError(t, err)
	require.Equal(t, []tcpConnection{{remote: mustAddr("1.1.1.1"), state: "ESTABLISHED"}}, connections)
}

func TestDecodeProcAddressIPv6(t *testing.T) {
	addr, err := decodeProcAddress("B80D0120000000000000000001000000:01BB", true)
	require.NoError(t, err)
	require.Equal(t, "2001:db8::1", addr.String())
}

func mustAddr(value string) netip.Addr {
	return netip.MustParseAddr(value)
}
