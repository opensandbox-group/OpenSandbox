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

package nftables

import (
	"os"
	"strconv"
	"strings"
	"time"
)

func readConntrackFlows() ([]byte, error) {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// conntrackEstablishedTimeoutPath is the kernel's idle timeout for established
// TCP conntrack entries; a var so tests can point it elsewhere.
var conntrackEstablishedTimeoutPath = "/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"

// establishedCarryTTL bounds how long a dropEstablished teardown set is
// carried across later full applies: once the conntrack established timeout
// has passed, any flow the drop targeted has been idle long enough to expire.
func establishedCarryTTL() time.Duration {
	data, err := os.ReadFile(conntrackEstablishedTimeoutPath)
	if err != nil {
		return defaultEstablishedCarryTTL
	}
	secs, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || secs <= 0 {
		return defaultEstablishedCarryTTL
	}
	return time.Duration(secs) * time.Second
}
