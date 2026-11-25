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
)

const (
	protoTCP          = 6
	protoUDP          = 17
	maxIPv6ExtHeaders = 8
)

// flow is the copied packet's 5-tuple, not a unique connection identifier.
type flow struct {
	src, dst     netip.Addr
	sport, dport uint16
	proto        uint8
}

func (f flow) protocol() string {
	if f.proto == protoTCP {
		return "tcp"
	}
	return "udp"
}

// decodeFlow reads just the addresses, transport and ports. NFLOG can truncate
// the original packet. Unsupported transports and later fragments have no
// usable ports; too-long IPv6 header chains are deliberately not walked.
func decodeFlow(pkt []byte) (flow, bool) {
	var f flow
	if len(pkt) == 0 {
		return f, false
	}
	var proto uint8
	var offset int
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return f, false
		}
		offset = int(pkt[0]&0x0f) * 4
		if offset < 20 || binary.BigEndian.Uint16(pkt[6:8])&0x1fff != 0 {
			return f, false
		}
		proto = pkt[9]
		f.src = netip.AddrFrom4([4]byte(pkt[12:16]))
		f.dst = netip.AddrFrom4([4]byte(pkt[16:20]))
	case 6:
		if len(pkt) < 40 {
			return f, false
		}
		offset = 40
		proto = pkt[6]
		f.src = netip.AddrFrom16([16]byte(pkt[8:24]))
		f.dst = netip.AddrFrom16([16]byte(pkt[24:40]))
		for i := 0; ; i++ {
			var size int
			switch proto {
			case 0, 43, 60: // Hop-by-hop, routing and destination options.
				if len(pkt) < offset+2 {
					return f, false
				}
				size = (int(pkt[offset+1]) + 1) * 8
			case 44: // Fragment; the first fragment has offset zero.
				if len(pkt) < offset+8 || binary.BigEndian.Uint16(pkt[offset+2:offset+4])&0xfff8 != 0 {
					return f, false
				}
				size = 8
			}
			if size == 0 {
				break
			}
			if i == maxIPv6ExtHeaders {
				return f, false
			}
			proto = pkt[offset]
			offset += size
		}
	default:
		return f, false
	}
	if (proto != protoTCP && proto != protoUDP) || len(pkt) < offset+4 {
		return f, false
	}
	f.proto = proto
	f.sport = binary.BigEndian.Uint16(pkt[offset : offset+2])
	f.dport = binary.BigEndian.Uint16(pkt[offset+2 : offset+4])
	return f, true
}
