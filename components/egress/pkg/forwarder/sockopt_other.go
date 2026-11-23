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

//go:build !linux

package forwarder

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

func originalDst(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("SO_ORIGINAL_DST is only supported on linux")
}

// MarkControl sets SO_MARK on the dialed socket on Linux.
func MarkControl(mark int) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(string, string, syscall.RawConn) error {
		return errors.New("SO_MARK is only supported on linux")
	}
}
