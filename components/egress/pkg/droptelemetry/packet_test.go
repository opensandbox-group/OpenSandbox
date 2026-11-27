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

package droptelemetry

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePacket(t *testing.T) {
	for _, v := range []byte{4, 6} {
		for _, proto := range []byte{6, 17} {
			off := 20
			if v == 6 {
				off = 40
			}
			b := make([]byte, off+20)
			b[0] = v<<4 | 5
			if v == 4 {
				b[9] = proto
				copy(b[12:16], []byte{10, 0, 0, 1})
				copy(b[16:20], []byte{192, 0, 2, 1})
				binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
			} else {
				b[6] = proto
				b[23] = 1
				b[39] = 2
				binary.BigEndian.PutUint16(b[4:6], 20)
			}
			b[off+2] = 1
			b[off+3] = 187
			b[off+12] = 5 << 4
			p, ok := ParsePacket(b)
			require.True(t, ok)
			require.EqualValues(t, 443, p.DstPort)
			require.True(t, p.SrcIP.IsValid())
			for i := 0; i < off+8; i++ {
				_, ok := ParsePacket(b[:i])
				require.False(t, ok)
			}
			if v == 4 {
				b[7] = 1
			} else {
				b[6] = 44
				b[off+2] = 0
				b[off+3] = 8
			}
			_, ok = ParsePacket(b)
			require.False(t, ok)
		}
	}
}
func FuzzParsePacket(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 128))
	f.Fuzz(func(t *testing.T, b []byte) { ParsePacket(b) })
}

func TestParseIPv6Extensions(t *testing.T) {
	b := make([]byte, 68)
	b[0] = 0x60
	b[6] = 0
	b[40] = 6
	b[41] = 0
	b[50] = 0x01
	b[51] = 0xbb
	b[60] = 0x50
	p, ok := ParsePacket(b)
	require.True(t, ok)
	require.Equal(t, "tcp", p.Proto)
	require.EqualValues(t, 443, p.DstPort)
	b[41] = 20
	_, ok = ParsePacket(b)
	require.False(t, ok)
	b[6] = 44
	b[41] = 0
	b[42] = 0
	b[43] = 0
	_, ok = ParsePacket(b)
	require.True(t, ok)
	b[43] = 8
	_, ok = ParsePacket(b)
	require.False(t, ok)
}
