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
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

type tcpConnection struct {
	remote netip.Addr
	state  string
}

var tcpStates = map[string]string{
	"01": "ESTABLISHED",
	"02": "SYN_SENT",
	"03": "SYN_RECV",
	"04": "FIN_WAIT1",
	"05": "FIN_WAIT2",
	"08": "CLOSE_WAIT",
	"09": "LAST_ACK",
	"0B": "CLOSING",
}

func listTCPConnections(ctx context.Context) ([]tcpConnection, error) {
	var connections []tcpConnection
	for _, file := range []struct {
		path string
		ipv6 bool
	}{
		{path: "/proc/net/tcp"},
		{path: "/proc/net/tcp6", ipv6: true},
	} {
		parsed, err := readTCPConnections(ctx, file.path, file.ipv6)
		if err != nil {
			if file.ipv6 && os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		connections = append(connections, parsed...)
	}
	return connections, nil
}

func readTCPConnections(ctx context.Context, path string, ipv6 bool) ([]tcpConnection, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var connections []tcpConnection
	scanner := bufio.NewScanner(file)
	if scanner.Scan() { // header
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		state, ok := tcpStates[strings.ToUpper(fields[3])]
		if !ok {
			continue
		}
		remote, err := decodeProcAddress(fields[2], ipv6)
		if err != nil || remote.IsUnspecified() {
			continue
		}
		connections = append(connections, tcpConnection{remote: remote, state: state})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return connections, nil
}

func decodeProcAddress(value string, ipv6 bool) (netip.Addr, error) {
	hexIP, _, ok := strings.Cut(value, ":")
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid proc address %q", value)
	}
	bytes, err := hex.DecodeString(hexIP)
	if err != nil {
		return netip.Addr{}, err
	}
	if ipv6 {
		if len(bytes) != 16 {
			return netip.Addr{}, fmt.Errorf("invalid IPv6 address %q", hexIP)
		}
		for i := 0; i < len(bytes); i += 4 {
			reverseBytes(bytes[i : i+4])
		}
	} else {
		if len(bytes) != 4 {
			return netip.Addr{}, fmt.Errorf("invalid IPv4 address %q", hexIP)
		}
		reverseBytes(bytes)
	}
	addr, ok := netip.AddrFromSlice(bytes)
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid IP address %q", hexIP)
	}
	return addr.Unmap(), nil
}

func reverseBytes(bytes []byte) {
	for i, j := 0, len(bytes)-1; i < j; i, j = i+1, j-1 {
		bytes[i], bytes[j] = bytes[j], bytes[i]
	}
}
