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

//go:build linux

package forwarder

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// originalDst reads the pre-REDIRECT IPv4 destination (SO_ORIGINAL_DST).
func originalDst(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var mreq *unix.IPv6Mreq
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		// SO_ORIGINAL_DST returns a sockaddr_in; IPv6Mreq is the usual
		// 16+ byte carrier for it in x/sys.
		mreq, opErr = unix.GetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IP, unix.SO_ORIGINAL_DST)
	}); err != nil {
		return netip.AddrPort{}, err
	}
	if opErr != nil {
		return netip.AddrPort{}, fmt.Errorf("SO_ORIGINAL_DST: %w", opErr)
	}
	b := mreq.Multiaddr
	port := uint16(b[2])<<8 | uint16(b[3])
	addr := netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]})
	return netip.AddrPortFrom(addr, port), nil
}

// MarkControl sets SO_MARK on the dialed socket so nft can scope trusted
// sidecar traffic (forwarder gateway or OTLP telemetry). mark 0 leaves the
// socket unmarked.
func MarkControl(mark int) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		if err := c.Control(func(fd uintptr) {
			opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
		}); err != nil {
			return err
		}
		if opErr != nil {
			return fmt.Errorf("set SO_MARK: %w", opErr)
		}
		return nil
	}
}
