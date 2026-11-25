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

package nftables

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func copiedIPv4(proto uint8, dstPort uint16) []byte {
	p := [28]byte{0x45, 0, 0, 28, 0, 0, 0, 0, 64, proto, 0, 0, 192, 0, 2, 2, 198, 51, 100, 7}
	binary.BigEndian.PutUint16(p[20:22], 40000)
	binary.BigEndian.PutUint16(p[22:24], dstPort)
	return p[:]
}

func copiedIPv6(next uint8, ext []byte) []byte {
	p := make([]byte, 40+len(ext)+8)
	p[0], p[6] = 0x60, next
	src := netip.MustParseAddr("2001:db8::2").As16()
	dst := netip.MustParseAddr("2001:db8::7").As16()
	copy(p[8:24], src[:])
	copy(p[24:40], dst[:])
	copy(p[40:], ext)
	binary.BigEndian.PutUint16(p[40+len(ext):], 50000)
	binary.BigEndian.PutUint16(p[42+len(ext):], 443)
	return p
}

func TestDecodeL4Tuple(t *testing.T) {
	v4 := copiedIPv4(protoTCP, 8443)
	v6 := copiedIPv6(0, []byte{44, 0, 0, 0, 0, 0, 0, 0, protoUDP, 0, 0, 0, 0, 0, 0, 1})
	for _, tt := range []struct {
		packet []byte
		min    int
		want   flow
	}{
		{v4, 24, flow{src: netip.MustParseAddr("192.0.2.2"), dst: netip.MustParseAddr("198.51.100.7"), sport: 40000, dport: 8443, proto: protoTCP}},
		{v6, 60, flow{src: netip.MustParseAddr("2001:db8::2"), dst: netip.MustParseAddr("2001:db8::7"), sport: 50000, dport: 443, proto: protoUDP}},
	} {
		for n := range tt.min {
			_, ok := decodeFlow(tt.packet[:n])
			require.False(t, ok, "truncated to %d bytes", n)
		}
		got, ok := decodeFlow(tt.packet[:tt.min])
		require.True(t, ok, "a copy need only include the ports")
		require.Equal(t, tt.want, got)
	}
}

func TestDecodeL4RejectsUnsupportedHeaders(t *testing.T) {
	badIHL := copiedIPv4(protoTCP, 42)
	badIHL[0] = 0x44
	fragV4 := copiedIPv4(protoTCP, 42)
	fragV4[7] = 1
	var extensions []byte
	for range maxIPv6ExtHeaders {
		extensions = append(extensions, 60, 0, 0, 0, 0, 0, 0, 0)
	}
	extensions = append(extensions, protoUDP, 0, 0, 0, 0, 0, 0, 0)
	for _, pkt := range [][]byte{
		badIHL, fragV4, copiedIPv4(1, 0),
		copiedIPv6(44, []byte{protoTCP, 0, 0, 8, 0, 0, 0, 1}),
		copiedIPv6(60, extensions),
		copiedIPv6(0, []byte{protoTCP, 4}),
	} {
		_, ok := decodeFlow(pkt)
		require.False(t, ok, "invalid headers: %x", pkt)
	}
}
