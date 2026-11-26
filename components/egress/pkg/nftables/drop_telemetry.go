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
	switch {
	case strings.Contains(line, "ct state established,related"):
		return droptelemetry.EstablishedRevoke
	case strings.Contains(line, "dport 853"):
		return droptelemetry.DoT
	case strings.Contains(line, "dport 443"):
		return droptelemetry.DoH
	case strings.Contains(line, "daddr @deny_"):
		return droptelemetry.DenySet
	case strings.Contains(line, "tcp dport {"):
		return droptelemetry.PortDeny
	case line == "add rule inet "+tableName+" "+chainName+" drop":
		return droptelemetry.DefaultDeny
	}
	return ""
}

func RunDropProbe(ctx context.Context, script string) ([]byte, error) {
	return defaultRunner(ctx, script)
}
