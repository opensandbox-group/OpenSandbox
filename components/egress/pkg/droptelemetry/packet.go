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
	"net/netip"
)

type Packet struct {
	SrcIP, DstIP netip.Addr
	DstPort      uint16
	Proto        string
	Reason       Reason
}

// ParsePacket accepts only complete transport headers from first fragments.
// Extension headers are bounded by the 128-byte NFLOG capture length.
func ParsePacket(b []byte) (Packet, bool) {
	var p Packet
	if len(b) < 1 {
		return p, false
	}
	off := 0
	proto := byte(0)
	switch b[0] >> 4 {
	case 4:
		if len(b) < 20 || b[0]&15 < 5 {
			return p, false
		}
		off = int(b[0]&15) * 4
		if len(b) < off || binary.BigEndian.Uint16(b[6:8])&0x1fff != 0 {
			return p, false
		}
		p.SrcIP = netip.AddrFrom4([4]byte(b[12:16]))
		p.DstIP = netip.AddrFrom4([4]byte(b[16:20]))
		proto = b[9]
	case 6:
		if len(b) < 40 {
			return p, false
		}
		p.SrcIP = netip.AddrFrom16([16]byte(b[8:24]))
		p.DstIP = netip.AddrFrom16([16]byte(b[24:40]))
		off = 40
		proto = b[6]
		for proto == 0 || proto == 43 || proto == 60 || proto == 44 || proto == 51 {
			if len(b) < off+8 {
				return p, false
			}
			next := b[off]
			size := (int(b[off+1]) + 1) * 8
			if proto == 44 {
				if binary.BigEndian.Uint16(b[off+2:off+4])&0xfff8 != 0 {
					return p, false
				}
				size = 8
			}
			if proto == 51 {
				size = (int(b[off+1]) + 2) * 4
			}
			off += size
			proto = next
			if off > len(b) {
				return p, false
			}
		}
	default:
		return p, false
	}
	size := 8
	switch proto {
	case 6:
		p.Proto = "tcp"
		size = 20
	case 17:
		p.Proto = "udp"
	default:
		return p, false
	}
	if len(b) < off+size {
		return p, false
	}
	if proto == 6 {
		hlen := int(b[off+12]>>4) * 4
		if hlen < 20 || len(b) < off+hlen {
			return p, false
		}
	}
	p.DstPort = binary.BigEndian.Uint16(b[off+2 : off+4])
	return p, true
}
