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
	"context"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
)

func dropReason(line string) droptelemetry.Reason {
	f := strings.Fields(line)
	switch {
	case strings.Contains(line, "ct state established,related"):
		return droptelemetry.EstablishedRevoke
	case strings.Contains(line, "tcp dport {") || strings.Contains(line, "proto-dst {"):
		return droptelemetry.PortDeny
	case hasDropPort(f, "853"):
		return droptelemetry.DoT
	case hasDropPort(f, "443"):
		return droptelemetry.DoH
	case strings.Contains(line, "daddr @deny_"):
		return droptelemetry.DenySet
	case line == "add rule inet "+tableName+" "+chainName+" drop":
		return droptelemetry.DefaultDeny
	}
	return ""
}

func RunDropProbe(ctx context.Context, script string) ([]byte, error) {
	return defaultRunner(ctx, script)
}

func hasDropPort(fields []string, port string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if (fields[i] == "dport" || fields[i] == "proto-dst") && fields[i+1] == port {
			return true
		}
	}
	return false
}
